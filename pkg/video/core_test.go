package video

import (
	"context"
	"sync"
	"testing"
	"time"

	"nvr/pkg/log"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/hls"

	"github.com/stretchr/testify/require"
)

type cancelFunc func()

type stubHlsServer struct{}

func (*stubHlsServer) setPathManager(*pathManager) {}

func (*stubHlsServer) muxerCreate(pathID, log.Func, gortsplib.Tracks) (*hls.Muxer, error) {
	return nil, nil
}

func (*stubHlsServer) muxerDestroy(pathID) {}

func (*stubHlsServer) muxerByPathName(string) (*hls.Muxer, error) {
	return nil, nil
}

func newTestServer(t *testing.T) (*Server, cancelFunc) {
	t.Helper()
	wg := sync.WaitGroup{}

	logger := log.NewDummyLogger()
	pathManager := newPathManager(&wg, logger, &stubHlsServer{})

	s := &Server{
		rtspAddress: "127.0.0.1:8554",
		hlsAddress:  "127.0.0.1:8888",
		pathManager: pathManager,
		wg:          &wg,
	}

	cancelFunc := func() {
		wg.Wait()
	}

	return s, cancelFunc
}

func TestNewPath(t *testing.T) {
	p, cancel := newTestServer(t)
	defer cancel()

	c, err := NewPathConf("x", false)
	require.NoError(t, err)

	ctx, cancel2 := context.WithCancel(context.Background())
	actual, err := p.NewPath(ctx, "mypath", *c)
	require.NoError(t, err)
	actual.HLSMuxer = nil

	expected := ServerPath{
		HlsAddress:   "http://127.0.0.1:8888/hls/mypath/index.m3u8",
		RtspAddress:  "rtsp://127.0.0.1:8554/mypath",
		RtspProtocol: "tcp",
	}
	require.Equal(t, expected, *actual)

	require.True(t, p.PathExist("mypath"))

	cancel2()
	time.Sleep(10 * time.Millisecond)
	require.False(t, p.PathExist("mypath"))
}
