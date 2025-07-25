package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/flutter-webrtc/flutter-whip-server/pkg/util"

	"github.com/flutter-webrtc/flutter-whip-server/pkg/whip"
	"github.com/gorilla/mux"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/spf13/viper"
)

type Config struct {
	whip.Config `mapstructure:",squash"`
}

var (
	conf     Config
	file     = ""
	addr     = ":8080"
	cert     = ""
	key      = ""
	webRoot  = "html"
	listLock sync.RWMutex
	conns    = make(map[string]*whipState)
)

func addTrack(w *whipState, t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
	}()

	trackId := t.ID()
	if trackId == "" {
		// If the track ID is empty, generate a new one
		trackId = util.RandomString(12)
		log.Printf("Track ID is empty, generated new one: %v", trackId)
	} else {
		log.Printf("Track ID is: %v", trackId)
	}

	streamId := t.StreamID()
	if streamId == "" {
		// If the stream ID is empty, generate a new one
		streamId = "whip-stream"
		log.Printf("Stream ID is empty, generated new one: %v", streamId)
	} else {
		log.Printf("Stream ID is: %v", streamId)
	}

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, trackId, streamId)
	if err != nil {
		panic(err)
	}

	w.pubTracks[trackId] = trackLocal
	return trackLocal
}

func removeTrack(w *whipState, t *webrtc.TrackLocalStaticRTP) {
	listLock.Lock()
	defer func() {
		listLock.Unlock()
	}()

	delete(w.pubTracks, t.ID())
}

type whipState struct {
	stream    string
	room      string
	publish   bool
	whipConn  *whip.WHIPConn
	pubTracks map[string]*webrtc.TrackLocalStaticRTP
}

func showHelp() {
	fmt.Printf("Usage:%s {params}\n", os.Args[0])
	fmt.Println("      -c {config file}")
	fmt.Println("      -cert {cert file for https}")
	fmt.Println("      -key {key file for https}")
	fmt.Println("      -bind {bind listen addr}")
	fmt.Println("      -web {html root directory}")
	fmt.Println("      -h (show help info)")
}

func load(file string) bool {
	_, err := os.Stat(file)
	if err != nil {
		return false
	}

	viper.SetConfigFile(file)
	viper.SetConfigType("toml")

	err = viper.ReadInConfig()
	if err != nil {
		log.Print("config file read failed ", err, " file", file)
		return false
	}
	err = viper.GetViper().Unmarshal(&conf)
	if err != nil {
		log.Print("whip config file loaded failed ", err, " file", file)
		return false
	}
	return true
}

func printWhipState() {
	log.Printf("State for whip:")
	for key, conn := range conns {
		streamType := "\tpublisher"
		if !conn.publish {
			streamType = "\tsubscriber"
		}
		log.Printf("%v: room: %v, stream: %v, resourceId: [%v]", streamType, conn.room, conn.stream, key)
	}
}

func main() {
	flag.StringVar(&file, "c", "config.toml", "config file")
	flag.StringVar(&cert, "cert", "", "cert file")
	flag.StringVar(&key, "key", "", "key file")
	flag.StringVar(&addr, "addr", ":8080", "http listening address")
	flag.StringVar(&webRoot, "web", "html", "html root directory")
	help := flag.Bool("h", false, "help info")
	flag.Parse()

	if !load(file) {
		return
	}

	if *help {
		showHelp()
		return
	}

	whip.Init(conf.Config)

	r := mux.NewRouter()

	r.HandleFunc("/whip/{mode}/{room}/{streamId}", func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		vars := mux.Vars(r)
		roomId := vars["room"]
		streamId := vars["streamId"]
		mode := vars["mode"]
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		log.Printf("Post: roomId => %v, streamId => %v, body = %v", roomId, streamId, string(body))

		listLock.Lock()
		defer listLock.Unlock()

		whip, err := whip.NewWHIPConn()

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			msg := "500 - failed to create whip conn!"
			log.Printf("%v", msg)
			w.Write([]byte(msg))
			return
		}

		if mode == "publish" {
			for _, wc := range conns {
				if wc.publish && wc.stream == streamId {
					w.WriteHeader(http.StatusInternalServerError)
					msg := "500 - publish conn [" + streamId + "] already exist!"
					log.Printf("%v", msg)
					w.Write([]byte(msg))
					return
				}
			}
		}

		state := &whipState{
			stream:    streamId,
			room:      roomId,
			publish:   mode == "publish",
			whipConn:  whip,
			pubTracks: make(map[string]*webrtc.TrackLocalStaticRTP),
		}

		if mode == "publish" {
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

				pubTrack := addTrack(state, track)
				defer removeTrack(state, pubTrack)

				buf := make([]byte, 1500)
				for {
					i, _, err := track.Read(buf)
					if err != nil {
						return
					}

					if _, err = pubTrack.Write(buf[:i]); err != nil {
						return
					}
				}
			}
		}

		if mode == "subscribe" {
			foundPublish := false
			for _, wc := range conns {
				if wc.publish && wc.stream == streamId {
					for trackID := range wc.pubTracks {
						if _, err := whip.AddTrack(wc.pubTracks[trackID]); err != nil {
							return
						}
					}
					go func() {
						time.Sleep(time.Second * 1)
						wc.whipConn.PictureLossIndication()
					}()
					foundPublish = true
				}
			}
			if !foundPublish {
				w.WriteHeader(http.StatusNotFound)
				msg := fmt.Sprintf("Not find any publisher for room: %v, stream: %v", roomId, streamId)
				log.Print(msg)
				w.Write([]byte(msg))
				return
			}
		}

		uniqueResourceId := mode + "-" + streamId + "-" + util.RandomString(12)

		conns[uniqueResourceId] = state

		log.Printf("got offer => %v", string(body))
		answer, err := whip.Offer(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(body)})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			msg := fmt.Sprintf("failed to answer whip conn: %v", err)
			log.Print(msg)
			w.Write([]byte(msg))
			return
		}
		log.Printf("send answer => %v", answer.SDP)
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
					streamType := "publish"
					if !state.publish {
						streamType = "subscribe"
					}
					log.Printf("%v stream conn removed  %v", streamType, streamId)
				}
			}
		}
		printWhipState()
	}).Methods("POST")

	r.HandleFunc("/whip/{room}/{resourceId}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		roomId := vars["room"]
		resourceId := vars["resourceId"]
		body, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		log.Printf("Patch: roomId => %v, resourceId => %v, body = %v", roomId, resourceId, string(body))
		listLock.Lock()
		defer listLock.Unlock()
		if state, found := conns[resourceId]; found {
			mid := "0"
			index := uint16(0)
			state.whipConn.AddICECandidate(webrtc.ICECandidateInit{Candidate: string(body), SDPMid: &mid, SDPMLineIndex: &index})
			w.Header().Set("Content-Type", "application/trickle-ice-sdpfrag")
			w.WriteHeader(http.StatusCreated)
		}
	}).Methods("PATCH")

	r.HandleFunc("/whip/{room}/{resourceId}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		roomId := vars["room"]
		resourceId := vars["stream"]

		log.Printf("Delete: roomId => %v, resourceId => %v", roomId, resourceId)

		listLock.Lock()
		defer listLock.Unlock()
		if state, found := conns[resourceId]; found {
			state.whipConn.Close()
			delete(conns, resourceId)
			connType := "publish"
			if !state.publish {
				connType = "subscribe"
			}
			log.Printf("%v stream conn removed  %v", connType, resourceId)
			printWhipState()
		} else {
			w.WriteHeader(http.StatusNotFound)
			msg := "resource " + resourceId + " not found"
			log.Print(msg)
			w.Write([]byte(msg))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(resourceId + " deleted"))
	}).Methods("DELETE")

	r.HandleFunc("/whip/list", func(w http.ResponseWriter, r *http.Request) {
		listLock.Lock()
		defer listLock.Unlock()
		list := make([]map[string]interface{}, 0)
		for key, item := range conns {
			details := make(map[string]interface{})
			connType := "publish"
			if !item.publish {
				connType = "subscribe"
			}
			details["path"] = item.room + "/" + item.stream
			details["type"] = connType
			details["uniqueID"] = key
			details["room"] = item.room
			details["stream"] = item.stream
			list = append(list, details)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(list)
	}).Methods("GET")

	r.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir(webRoot))))
	r.Headers("Access-Control-Allow-Origin", "*")

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
