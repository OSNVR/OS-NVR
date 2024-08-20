package video

import (
	"context"
	"errors"
	"fmt"
	"nvr/pkg/log"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/gortsplib/pkg/base"
	"nvr/pkg/video/hls"
	"regexp"
	"sync"
)

type pathManagerHLSServer interface {
	muxerCreate(pathID, log.Func, gortsplib.Tracks) (*hls.Muxer, error)
	muxerDestroy(pathID)
	muxerByPathName(string) (*hls.Muxer, error)
}

type pathManager struct {
	wg     *sync.WaitGroup
	logger log.ILogger
	mu     sync.Mutex

	hlsServer pathManagerHLSServer
	paths     map[string]*path

	nextPathID uint32
}

func newPathManager(
	wg *sync.WaitGroup,
	log log.ILogger,
	hlsServer pathManagerHLSServer,
) *pathManager {
	return &pathManager{
		wg:        wg,
		logger:    log,
		hlsServer: hlsServer,
		paths:     make(map[string]*path),
	}
}

// Errors.
var (
	ErrPathAlreadyExist = errors.New("path already exist")
	ErrPathNotExist     = errors.New("path not exist")
	ErrEmptyName        = errors.New("name can not be empty")
	ErrSlashStart       = errors.New("name can't begin with a slash")
	ErrSlashEnd         = errors.New("name can't end with a slash")
	ErrInvalidChars     = errors.New("can contain only alphanumeric" +
		" characters, underscore, dot, tilde, minus or slash")
)

var rePathName = regexp.MustCompile(`^[0-9a-zA-Z_\-/\.~]+$`)

func isValidPathName(name string) error {
	if name == "" {
		return ErrEmptyName
	}

	if name[0] == '/' {
		return ErrSlashStart
	}

	if name[len(name)-1] == '/' {
		return ErrSlashEnd
	}

	if !rePathName.MatchString(name) {
		return ErrInvalidChars
	}

	return nil
}

func (pm *pathManager) AddPath(ctx context.Context, name string, conf PathConf) (HlsMuxerFunc, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	err := isValidPathName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid path name: %s (%w)", name, err)
	}

	if _, exist := pm.paths[name]; exist {
		return nil, ErrPathAlreadyExist
	}

	ctx, cancel := context.WithCancel(ctx)

	// Add path.
	pa := &path{
		id:         pm.genPathID(),
		name:       name,
		conf:       conf,
		cancelFunc: cancel,
		readers:    make(map[*rtspSession]struct{}),
	}
	pm.paths[name] = pa

	hlsMuxer := func() (IHLSMuxer, error) {
		return pm.hlsServer.muxerByPathName(name)
	}

	pathID := pa.ID()
	pm.wg.Add(1)
	go func() {
		// Cancellation.
		<-ctx.Done()
		pm.pathCloseAndRemove(pathID)
	}()

	return hlsMuxer, nil
}

// Testing.
func (pm *pathManager) pathExist(name string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	_, exist := pm.paths[name]
	return exist
}

// onDescribe is called by a rtsp reader.
func (pm *pathManager) onDescribe(
	pathName string,
) (*base.Response, *gortsplib.ServerStream, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[pathName]
	if !exist {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, ErrPathNotExist
	}

	if !path.rtspSourceReady {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, ErrPathNoOnePublishing
	}
	return &base.Response{StatusCode: base.StatusOK}, path.stream.rtspStream, nil
}

// ErrPathBusy another publisher is aldreay publishing to path.
var ErrPathBusy = errors.New("another publisher is already publishing to path")

// pathPublisherAdd is called by a rtsp publisher.
func (pm *pathManager) pathPublisherAdd(
	name string,
	session *rtspSession,
) (*pathID, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[name]
	if !exist {
		return nil, ErrPathNotExist
	}

	if path.rtspSource != nil {
		return nil, ErrPathBusy
	}
	path.rtspSource = session

	return &pathID{id: path.id, name: path.name}, nil
}

// pathReaderAdd is called by a rtsp reader.
func (pm *pathManager) pathReaderAdd(
	name string,
	session *rtspSession,
) (*pathID, *stream, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[name]
	if !exist {
		return nil, nil, ErrPathNotExist
	}

	if !path.rtspSourceReady {
		return nil, nil, fmt.Errorf("%w: (%s)", ErrPathNoOnePublishing, path.name)
	}

	path.readers[session] = struct{}{}
	return &pathID{id: path.id, name: path.name}, path.stream, nil
}

func (pm *pathManager) pathLogfByName(name string) (log.Func, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pa, exist := pm.paths[name]
	if !exist {
		return nil, ErrPathNotExist
	}
	return newPathLogf(pm.logger, pa), nil
}

func (pm *pathManager) genPathID() uint32 {
	id := pm.nextPathID
	pm.nextPathID++
	return id
}

type pathID struct {
	id   uint32
	name string
}

// ErrPathNoOnePublishing No one is publishing to path.
var ErrPathNoOnePublishing = errors.New("no one is publishing to path")

// pathPublisherStart is called by a publisher.
func (pm *pathManager) pathPublisherStart(pathID pathID, tracks gortsplib.Tracks) (*stream, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[pathID.name]
	if !exist || path.id != pathID.id {
		return nil, ErrPathNotExist
	}

	hlsMuxer, err := pm.hlsServer.muxerCreate(pathID, newPathLogf(pm.logger, path), tracks)
	if err != nil {
		return nil, err
	}

	path.stream = newStream(tracks, hlsMuxer)
	path.rtspSourceReady = true

	return path.stream, err
}

func newPathLogf(logger log.ILogger, pa *path) log.Func {
	processName := func() string {
		if pa.conf.isSub {
			return "sub"
		}
		return "main"
	}()
	monitorID := pa.conf.monitorID

	return func(level log.Level, format string, a ...interface{}) {
		msg := fmt.Sprintf("%v: %v", processName, fmt.Sprintf(format, a...))
		logger.Log(log.Entry{
			Level:     level,
			Src:       "monitor",
			MonitorID: monitorID,
			Msg:       msg,
		})
	}
}

// pathReaderRemove is called by a rtsp session.
func (pm *pathManager) pathReaderRemove(pathID pathID, session *rtspSession) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[pathID.name]
	if !exist || path.id != pathID.id {
		return
	}

	delete(path.readers, session)
}

// pathReaderStart is called by a rtsp session.
func (pm *pathManager) pathReaderStart(pathID pathID, session *rtspSession) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	path, exist := pm.paths[pathID.name]
	if !exist || path.id != pathID.id {
		return ErrPathNotExist
	}

	path.readers[session] = struct{}{}
	return nil
}

func (pm *pathManager) pathCloseAndRemove(pathID pathID) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Make sure this path haven't already been removed.
	path, exist := pm.paths[pathID.name]
	if !exist || path.id != pathID.id {
		return
	}

	delete(pm.paths, pathID.name)

	path.rtspSourceReady = false
	if path.rtspSource != nil {
		path.rtspSource.close()
	}

	// Close source before stream.
	if path.stream != nil {
		path.stream.close()
		path.stream = nil
	}

	for r := range path.readers {
		r.close()
		delete(path.readers, r)
	}

	pm.hlsServer.muxerDestroy(pathID)

	pm.wg.Done()
}

type path struct {
	id         uint32
	name       string
	conf       PathConf
	cancelFunc func()

	rtspSource      *rtspSession
	rtspSourceReady bool
	stream          *stream
	readers         map[*rtspSession]struct{}
}

func (pa *path) ID() pathID {
	return pathID{
		id:   pa.id,
		name: pa.name,
	}
}

var ErrEmptyMonitorID = errors.New("MonitorID can not be empty")

func NewPathConf(monitorID string, isSub bool) (*PathConf, error) {
	if monitorID == "" {
		return nil, ErrEmptyMonitorID
	}

	return &PathConf{
		monitorID: monitorID,
		isSub:     isSub,
	}, nil
}

// PathConf is a path configuration.
type PathConf struct {
	monitorID string
	isSub     bool
}
