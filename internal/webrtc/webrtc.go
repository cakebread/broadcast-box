package webrtc

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/ice/v2"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

const (
	videoTrackLabelDefault = "default"

	videoTrackCodecH264 videoTrackCodec = iota + 1
	videoTrackCodecVP8
	videoTrackCodecVP9
	videoTrackCodecAV1
)

type (
	stream struct {
		// Does this stream have a publisher?
		// If stream was created by a WHEP request hasWHIPClient == false
		hasWHIPClient    atomic.Bool
		videoTrackLabels []string
		audioTrack       *webrtc.TrackLocalStaticRTP

		pliChan chan any

		whepSessionsLock sync.RWMutex
		whepSessions     map[string]*whepSession
	}

	videoTrackCodec int
)

var (
	streamMap        map[string]*stream
	streamMapLock    sync.Mutex
	apiWhip, apiWhep *webrtc.API

	// nolint
	videoRTCPFeedback = []webrtc.RTCPFeedback{{"goog-remb", ""}, {"ccm", "fir"}, {"nack", ""}, {"nack", "pli"}}
)

func getVideoTrackCodec(in string) videoTrackCodec {
	downcased := strings.ToLower(in)
	switch {
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeH264)):
		return videoTrackCodecH264
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeVP8)):
		return videoTrackCodecVP8
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeVP9)):
		return videoTrackCodecVP9
	case strings.Contains(downcased, strings.ToLower(webrtc.MimeTypeAV1)):
		return videoTrackCodecAV1
	}

	return 0
}

func getStream(streamKey string, forWHIP bool) (*stream, error) {
	foundStream, ok := streamMap[streamKey]
	if !ok {
		audioTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
		if err != nil {
			return nil, err
		}

		foundStream = &stream{
			audioTrack:   audioTrack,
			pliChan:      make(chan any, 50),
			whepSessions: map[string]*whepSession{},
		}
		streamMap[streamKey] = foundStream
	}

	if forWHIP {
		foundStream.hasWHIPClient.Store(true)
	}

	return foundStream, nil
}

func deleteStream(streamKey string) {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	delete(streamMap, streamKey)
}

func addTrack(stream *stream, rid string) error {
	streamMapLock.Lock()
	defer streamMapLock.Unlock()

	for i := range stream.videoTrackLabels {
		if rid == stream.videoTrackLabels[i] {
			return nil
		}
	}

	stream.videoTrackLabels = append(stream.videoTrackLabels, rid)
	return nil
}

func getPublicIP() string {
	req, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		log.Fatal(err)
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Fatal(err)
	}

	ip := struct {
		Query string
	}{}
	if err = json.Unmarshal(body, &ip); err != nil {
		log.Fatal(err)
	}

	if ip.Query == "" {
		log.Fatal("Query entry was not populated")
	}

	return ip.Query
}

func createSettingEngine(isWHIP bool, udpMuxCache map[int]*ice.MultiUDPMuxDefault) (settingEngine webrtc.SettingEngine) {
	var (
		NAT1To1IPs []string
		udpMuxPort int
		udpMuxOpts []ice.UDPMuxFromPortOption
		err        error
	)

	if os.Getenv("INCLUDE_PUBLIC_IP_IN_NAT_1_TO_1_IP") != "" {
		NAT1To1IPs = append(NAT1To1IPs, getPublicIP())
	}

	if os.Getenv("NAT_1_TO_1_IP") != "" {
		NAT1To1IPs = append(NAT1To1IPs, os.Getenv("NAT_1_TO_1_IP"))
	}

	if len(NAT1To1IPs) != 0 {
		settingEngine.SetNAT1To1IPs(NAT1To1IPs, webrtc.ICECandidateTypeHost)
	}

	if os.Getenv("INTERFACE_FILTER") != "" {
		interfaceFilter := func(i string) bool {
			return i == os.Getenv("INTERFACE_FILTER")
		}

		settingEngine.SetInterfaceFilter(interfaceFilter)
		udpMuxOpts = append(udpMuxOpts, ice.UDPMuxFromPortWithInterfaceFilter(interfaceFilter))
	}

	if isWHIP && os.Getenv("UDP_MUX_PORT_WHIP") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT_WHIP")); err != nil {
			log.Fatal(err)
		}
	} else if !isWHIP && os.Getenv("UDP_MUX_PORT_WHEP") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT_WHEP")); err != nil {
			log.Fatal(err)
		}
	} else if os.Getenv("UDP_MUX_PORT") != "" {
		if udpMuxPort, err = strconv.Atoi(os.Getenv("UDP_MUX_PORT")); err != nil {
			log.Fatal(err)
		}
	}

	if udpMuxPort != 0 {
		udpMux, ok := udpMuxCache[udpMuxPort]
		if !ok {
			if udpMux, err = ice.NewMultiUDPMuxFromPort(udpMuxPort, udpMuxOpts...); err != nil {
				log.Fatal(err)
			}
			udpMuxCache[udpMuxPort] = udpMux
		}

		settingEngine.SetICEUDPMux(udpMux)
	}

	if os.Getenv("TCP_MUX_ADDRESS") != "" {
		tcpAddr, err := net.ResolveTCPAddr("udp", os.Getenv("TCP_MUX_ADDRESS"))
		if err != nil {
			log.Fatal(err)
		}

		tcpListener, err := net.ListenTCP("tcp", tcpAddr)
		if err != nil {
			log.Fatal(err)
		}

		settingEngine.SetICETCPMux(webrtc.NewICETCPMux(nil, tcpListener, 8))
	}

	return
}

func populateMediaEngine(m *webrtc.MediaEngine) error {
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			// nolint
			RTPCodecCapability: webrtc.RTPCodecCapability{webrtc.MimeTypeOpus, 48000, 2, "minptime=10;useinbandfec=1", nil},
			PayloadType:        111,
		},
	} {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return err
		}
	}

	for _, codecDetails := range []struct {
		payloadType uint8
		mimeType    string
		sdpFmtpLine string
	}{
		{96, webrtc.MimeTypeVP8, ""},
		{102, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
		{104, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f"},
		{106, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		{108, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f"},
		{39, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f"},
		{45, webrtc.MimeTypeAV1, ""},
		{98, webrtc.MimeTypeVP9, "profile-id=0"},
		{100, webrtc.MimeTypeVP9, "profile-id=2"},
		{112, webrtc.MimeTypeH264, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},
	} {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     codecDetails.mimeType,
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  codecDetails.sdpFmtpLine,
				RTCPFeedback: videoRTCPFeedback,
			},
			PayloadType: webrtc.PayloadType(codecDetails.payloadType),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}

		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     "video/rtx",
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  fmt.Sprintf("apt=%d", codecDetails.payloadType),
				RTCPFeedback: nil,
			},
			PayloadType: webrtc.PayloadType(codecDetails.payloadType + 1),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
	}

	return nil
}

func newPeerConnection(api *webrtc.API) (*webrtc.PeerConnection, error) {
	cfg := webrtc.Configuration{}

	if stunServers := os.Getenv("STUN_SERVERS"); stunServers != "" {
		for _, stunServer := range strings.Split(stunServers, "|") {
			cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
				URLs: []string{"stun:" + stunServer},
			})
		}
	}

	return api.NewPeerConnection(cfg)
}

func Configure() {
	streamMap = map[string]*stream{}

	mediaEngine := &webrtc.MediaEngine{}
	if err := populateMediaEngine(mediaEngine); err != nil {
		panic(err)
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(mediaEngine, interceptorRegistry); err != nil {
		log.Fatal(err)
	}

	udpMuxCache := map[int]*ice.MultiUDPMuxDefault{}

	apiWhip = webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(createSettingEngine(true, udpMuxCache)),
	)

	apiWhep = webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
		webrtc.WithSettingEngine(createSettingEngine(false, udpMuxCache)),
	)
}
