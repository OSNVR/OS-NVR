package video

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"nvr/pkg/log"
	"nvr/pkg/video/hls"
	gopath "path"
	"strings"
	"sync"
	"time"
)

type hlsServer struct {
	address         string
	readBufferCount int
	pathManager     *pathManager
	logger          *log.Logger

	ctx       context.Context
	ctxCancel func()
	wg        *sync.WaitGroup
	muxers    map[string]*hlsMuxer

	// in
	pathSourceReady chan *path
	request         chan *hlsMuxerRequest
	muxerClose      chan *hlsMuxer
}

func newHLSServer(
	wg *sync.WaitGroup,
	address string,
	readBufferCount int,
	pathManager *pathManager,
	logger *log.Logger,
) *hlsServer {
	s := &hlsServer{
		address:         address,
		readBufferCount: readBufferCount,
		pathManager:     pathManager,
		logger:          logger,
		wg:              wg,
		muxers:          make(map[string]*hlsMuxer),
		pathSourceReady: make(chan *path),
		request:         make(chan *hlsMuxerRequest),
		muxerClose:      make(chan *hlsMuxer),
	}
	s.pathManager.onHLSServerSet(s)
	return s
}

func (s *hlsServer) start(ctx context.Context) error {
	s.ctx, s.ctxCancel = context.WithCancel(ctx)

	ln, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.logger.Info().
		Src("app").
		Msgf("HLS: listener opened on %v", s.address)

	s.wg.Add(2)
	s.startServer(ln)
	go s.run()

	return nil
}

// Log is the main logging function.
/*func (s *hlsServer) logf(level log.Level, format string, args ...interface{}) {
	_ = level
	fmt.Printf("[HLS] "+format+"\n", append([]interface{}{}, args...)...)
}*/

func (s *hlsServer) startServer(ln net.Listener) {
	mux := http.NewServeMux()
	mux.Handle("/hls/", s.HandleRequest())
	server := http.Server{Handler: mux}

	go func() {
		for {
			err := server.Serve(ln)
			if !errors.Is(err, http.ErrServerClosed) {
				s.logger.Error().
					Src("app").
					Msgf("hls: server stopped: %v\nrestarting..", err)

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
}

func (s *hlsServer) run() {
	defer s.wg.Done()

outer:
	for {
		select {
		case pa := <-s.pathSourceReady:
			s.findOrCreateMuxer(pa.Name(), nil)

		case req := <-s.request:
			s.findOrCreateMuxer(req.path, req)

		case c := <-s.muxerClose:
			if c2, ok := s.muxers[c.pathName]; !ok || c2 != c {
				continue
			}
			delete(s.muxers, c.pathName)

		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()
}

func (s *hlsServer) HandleRequest() http.HandlerFunc { //nolint:funlen
	return func(w http.ResponseWriter, r *http.Request) {
		// s.logf(log.LevelInfo, "[conn %v] %s %s", r.RemoteAddr, r.Method, r.URL.Path)

		w.Header().Set("Server", "rtsp-simple-server")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		switch r.Method {
		case http.MethodGet:

		case http.MethodOptions:
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

		cres := make(chan func() *hls.MuxerFileResponse)
		hreq := &hlsMuxerRequest{
			path: dir,
			file: fname,
			req:  r,
			res:  cres,
		}

		select {
		case <-s.ctx.Done():
		case s.request <- hreq:
			cb := <-cres

			res := cb()

			for k, v := range res.Header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(res.Status)

			if res.Body != nil {
				io.Copy(w, res.Body) //nolint:errcheck
			}
		}
	}
}

func (s *hlsServer) findOrCreateMuxer(pathName string, req *hlsMuxerRequest) {
	r, ok := s.muxers[pathName]
	if !ok {
		r = newHLSMuxer(
			s.ctx,
			pathName,
			s.readBufferCount,
			req,
			s.wg,
			pathName,
			s.pathManager,
			s,
			s.logger)
		s.muxers[pathName] = r
	} else if req != nil {
		r.onRequest(req)
	}
}

// onMuxerClose is called by hlsMuxer.
func (s *hlsServer) onMuxerClose(c *hlsMuxer) {
	select {
	case s.muxerClose <- c:
	case <-s.ctx.Done():
	}
}

// onPathSourceReady is called by core.
func (s *hlsServer) onPathSourceReady(pa *path) {
	select {
	case s.pathSourceReady <- pa:
	case <-s.ctx.Done():
	}
}
