package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/flutter-webrtc/flutter-whip-server/pkg/util"

	"github.com/flutter-webrtc/flutter-whip-server/internal/gst-sink"
	gst_sink "github.com/flutter-webrtc/flutter-whip-server/internal/gst-sink"
	"github.com/flutter-webrtc/flutter-whip-server/pkg/whip"
	"github.com/gorilla/mux"
	"github.com/mdp/qrterminal/v3"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
)

var (
	addr     = ":8080"
	cert     = ""
	key      = ""
	webRoot  = "html"
	rtmpSrv  = "localhost"
	vcodec   = "h264"
	rtmpmode = "pub"

	listLock sync.RWMutex
	conns    = make(map[string]*whipState)
)

type whipState struct {
	id       string
	whipConn *whip.WHIPConn
	pipeline *gst_sink.Pipeline
}

func newWhipState(id string, whip *whip.WHIPConn) *whipState {
	return &whipState{
		id:       id,
		whipConn: whip,
	}
}

func printQR(url string) {
	config := qrterminal.Config{
		Level:     qrterminal.L,
		Writer:    os.Stdout,
		BlackChar: qrterminal.BLACK,
		WhiteChar: qrterminal.WHITE,
		QuietZone: 1,
	}
	fmt.Println("WHIP publish QR Code:")
	qrterminal.GenerateWithConfig(url, config)
}

func getClientIp() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}
	return "", errors.New("Can not find the client ip address!")
}

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -cert {cert file for https}")
	fmt.Println("      -key {key file for https}")
	fmt.Println("      -bind {bind listen addr}")
	fmt.Println("      -web {html root directory}")
	fmt.Println("      -h (show help info)")
}

func main() {
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "addr", ":8080", "http listening address")
	flag.StringVar(&webRoot, "web", "html", "html root directory")
	flag.StringVar(&rtmpmode, "rtmpmode", "pub", "rtmp mode pub | sub")
	flag.StringVar(&rtmpSrv, "rtmp", "localhost", "rtmp server address")
	flag.StringVar(&vcodec, "vcodec", "vp8", "video codec vp8/vp9/h264")
	help := flag.Bool("h", false, "help info")
	flag.Parse()

	if *help {
		showHelp()
		return
	}

	r := mux.NewRouter()

	r.HandleFunc("/whip/{mode}/{room}/{stream}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		roomId := vars["room"]
		streamId := vars["stream"]
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		rtmpUrl := "rtmp://" + rtmpSrv + "/" + roomId + "/" + streamId
		log.Printf("Post: roomId => %v, streamId => %v, body = %v, publish to %v", roomId, streamId, string(body), rtmpUrl)

		listLock.Lock()
		defer listLock.Unlock()

		if _, found := conns[streamId]; !found {
			whip, err := whip.NewWHIPConn()

			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("500 - failed to create whip conn!"))
				return
			}

			state := newWhipState(streamId, whip)
			state.pipeline = gst.CreatePipeline(rtmpUrl, vcodec)
			state.pipeline.Start()

			whip.OnTrack = func(pc *webrtc.PeerConnection, track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {

				if track.Kind() == webrtc.RTPCodecTypeVideo {
					// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
					// This is a temporary fix until we implement incoming RTCP events, then we would push a PLI only when a viewer requests it
					go func() {
						ticker := time.NewTicker(time.Second * 3)
						for range ticker.C {
							errSend := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}})
							if errSend != nil {
								log.Println(errSend)
								return
							}
						}
					}()
				}
				mimeType := track.Codec().RTPCodecCapability.MimeType
				codecType := strings.Split(mimeType, "/")[0]

				buf := make([]byte, 1500)
				for {
					i, _, err := track.Read(buf)
					if err != nil {
						return
					}
					state.pipeline.Push(buf[:i], codecType)
				}
			}
			uniqueResourceId := streamId + "-" + util.RandomString(12)

			conns[uniqueResourceId] = state
			log.Printf("got offer => %v", string(body))
			answer, err := whip.Offer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(fmt.Sprintf("failed to answer whip conn: %v", err)))
				return
			}
			log.Printf("send answer => %v", answer.SDP)

			w.Header().Set("Content-Type", "application/sdp")
			w.Header().Set("Location", "/whip/"+roomId+"/"+uniqueResourceId)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(answer.SDP))

			whip.OnConnectionStateChange = func(state webrtc.PeerConnectionState) {
				if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
					listLock.Lock()
					defer listLock.Unlock()
					if state, found := conns[uniqueResourceId]; found {
						state.whipConn.Close()
						delete(conns, uniqueResourceId)
						log.Printf("%v stream conn removed", streamId)
					}
				}
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("stream " + streamId + " already exists"))
			return
		}
	}).Methods("POST", "GET")

	r.HandleFunc("/whip/{room}/{stream}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		roomId := vars["room"]
		streamId := vars["stream"]
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		log.Printf("Patch: roomId => %v, streamId => %v, body = %v", roomId, streamId, string(body))
		listLock.Lock()
		defer listLock.Unlock()
		if state, found := conns[streamId]; found {
			mid := "0"
			index := uint16(0)
			state.whipConn.AddICECandidate(webrtc.ICECandidateInit{Candidate: string(body), SDPMid: &mid, SDPMLineIndex: &index})

			w.Header().Set("Content-Type", "application/trickle-ice-sdpfrag")
			w.WriteHeader(http.StatusCreated)
		}
	}).Methods("PATCH")

	r.HandleFunc("/whip/{room}/{stream}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		roomId := vars["room"]
		streamId := vars["stream"]

		log.Printf("Delete: roomId => %v, streamId => %v", roomId, streamId)

		listLock.Lock()
		defer listLock.Unlock()
		if state, found := conns[streamId]; found {
			state.whipConn.Close()
			state.pipeline.Stop()
			delete(conns, streamId)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("stream " + streamId + " not found"))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(streamId + " deleted"))
	}).Methods("DELETE")

	r.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir(webRoot))))
	/*
		if localIp, err := getClientIp(); err == nil {
			printQR("http://" + localIp + addr + "/whip/live/stream1")
		}
	*/

	if cert != "" && key != "" {
		if e := http.ListenAndServeTLS(addr, cert, key, r); e != nil {
			log.Fatal("ListenAndServeTLS: ", e)
		}
	} else {
		if e := http.ListenAndServe(addr, r); e != nil {
			log.Fatal("ListenAndServe: ", e)
		}
	}
}
