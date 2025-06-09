# Demo Server for flutter whip.

* Support WebRTC to RTMP.
* One to many broadcast.

## Quickstart

Run example:

### one2many

```bash
git clone https://github.com/flutter-webrtc/flutter-whip-server.git
cd whip
go run examples/one2many/main.go
```

open <http://127.0.0.1:8080/>, Then you can run a publish, multiple subscribe pages.

### webrtc2rtmp

note: need to install gstreamer

```bash
git clone https://github.com/flutter-webrtc/flutter-whip-server.git
cd whip
# please ensure gstreamer is installed
# export PKG_CONFIG_PATH=/usr/local/lib/pkgconfig # for mac only
go run examples/webrtc2rtmp/main.go
# run any rtmp server
docker run --rm -it -p 1935:1935 -p 1985:1985 -p 8088:8080 \
        registry.cn-hangzhou.aliyuncs.com/ossrs/srs:4 ./objs/srs -c conf/docker.conf
```

open <http://127.0.0.1:8080/> and run the publish sample.

the you can play rtmp stream.

```bash
ffplay rtmp://127.0.0.1/live/stream1
```
