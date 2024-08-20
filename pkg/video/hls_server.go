package video

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"nvr/pkg/log"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/hls"
	gopath "path"
	"strings"
	"sync"
	"time"
)

type hlsServer struct {
	readBufferCount int
	logger          *log.Logger

	mu          sync.Mutex
	cancelled   bool
	ctx         context.Context
	wg          *sync.WaitGroup
	muxers      map[string]*hls.Muxer
	nextMuxerID uint16
}

func newHLSServer(
	wg *sync.WaitGroup,
	readBufferCount int,
	logger *log.Logger,
) *hlsServer {
	return &hlsServer{
		readBufferCount: readBufferCount,
		logger:          logger,
		mu:              sync.Mutex{},
		wg:              wg,
		muxers:          make(map[string]*hls.Muxer),
	}
}

func (s *hlsServer) start(ctx context.Context, address string) error {
	s.ctx = ctx

	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	s.logger.Log(log.Entry{
		Level: log.LevelInfo,
		Src:   "app",
		Msg:   fmt.Sprintf("HLS: listener opened on %v", address),
	})

	mux := http.NewServeMux()
	mux.Handle("/hls/", s.HandleRequestNoCors())
	server := http.Server{Handler: mux}

	s.wg.Add(1)
	go func() {
		for {
			err := server.Serve(ln)
			if !errors.Is(err, http.ErrServerClosed) {
				s.logger.Log(log.Entry{
					Level: log.LevelError,
					Src:   "app",
					Msg:   fmt.Sprintf("hls: server stopped: %v\nrestarting..", err),
				})
				time.Sleep(3 * time.Second)
			}
			if s.ctx.Err() != nil {
				return
			}
		}
	}()
	go func() {
		<-s.ctx.Done()
		server.Close()
		s.wg.Done()
	}()

	return nil
}

func (s *hlsServer) genMuxerID() uint16 {
	id := s.nextMuxerID
	s.nextMuxerID++
	return id
}

func (s *hlsServer) HandleRequestNoCors() http.HandlerFunc {
	inner := s.HandleRequest()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		inner.ServeHTTP(w, r)
	}
}

func (s *hlsServer) HandleRequest() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// s.logf(log.LevelInfo, "[conn %v] %s %s", r.RemoteAddr, r.Method, r.URL.Path)

		w.Header().Set("Server", "rtsp-simple-server")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		switch r.Method {
		case http.MethodGet:

		case http.MethodOptions:
			// BREAKING: move this to `HandleRequestNoCors`.
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
			w.WriteHeader(http.StatusOK)
			return

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		// Remove leading prefix "/hls/"
		if len(r.URL.Path) <= 5 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pa := r.URL.Path[5:]

		dir, fname := func() (string, string) {
			if strings.HasSuffix(pa, ".ts") ||
				strings.HasSuffix(pa, ".m3u8") ||
				strings.HasSuffix(pa, ".mp4") {
				return gopath.Dir(pa), gopath.Base(pa)
			}
			return pa, ""
		}()

		if fname == "" && !strings.HasSuffix(dir, "/") {
			w.Header().Set("Location", "/hls/"+dir+"/")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}

		dir = strings.TrimSuffix(dir, "/")

		res := s.onRequest(dir, fname, r)

		for k, v := range res.Header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(res.Status)

		if res.Body != nil {
			io.Copy(w, res.Body) //nolint:errcheck
		}
	}
}

func (s *hlsServer) onRequest(path string, file string, r *http.Request) hls.MuxerFileResponse {
	s.mu.Lock()
	if s.cancelled {
		s.mu.Unlock()
		return hls.MuxerFileReponseCancelled()
	}
	m, exist := s.muxers[path]
	if !exist {
		s.mu.Unlock()
		return hls.MuxerFileResponse{Status: http.StatusNotFound}
	}
	s.mu.Unlock() // Must be unlocked here.

	return m.OnRequest(file, r.URL.Query())
}

var ErrMuxerAleadyExists = errors.New("muxer already exists")

const (
	hlsSegmentCount    = 3
	hlsSegmentDuration = 900 * time.Millisecond
	hlsPartDuration    = 300 * time.Millisecond
)

var (
	mb                = uint64(1000000)
	hlsSegmentMaxSize = 50 * mb
)

func (s *hlsServer) muxerCreate(
	pathID pathID,
	pathLogf log.Func,
	tracks gortsplib.Tracks,
) (*hls.Muxer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return nil, context.Canceled
	}

	if _, exist := s.muxers[pathID.name]; exist {
		return nil, ErrMuxerAleadyExists
	}

	m, err := hls.NewMuxer(
		s.genMuxerID(),
		hlsSegmentCount,
		hlsSegmentDuration,
		hlsPartDuration,
		hlsSegmentMaxSize,
		pathLogf,
		tracks,
	)
	if err != nil {
		return nil, fmt.Errorf("new hls muxer: %w", err)
	}

	s.muxers[pathID.name] = m
	return m, nil
}

func (s *hlsServer) muxerDestroy(pathID pathID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return
	}

	if m, exist := s.muxers[pathID.name]; exist {
		m.Cancel()
		delete(s.muxers, pathID.name)
	}
}

func (s *hlsServer) muxerByPathName(pathName string) (*hls.Muxer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return nil, context.Canceled
	}

	m, exist := s.muxers[pathName]
	if !exist {
		return nil, context.Canceled
	}
	return m, nil
}
