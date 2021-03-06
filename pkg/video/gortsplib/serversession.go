package gortsplib

import (
	"context"
	"errors"
	"fmt"
	"nvr/pkg/video/gortsplib/pkg/base"
	"nvr/pkg/video/gortsplib/pkg/headers"
	"nvr/pkg/video/gortsplib/pkg/liberrors"
	"nvr/pkg/video/gortsplib/pkg/ringbuffer"
	"nvr/pkg/video/gortsplib/pkg/rtpcleaner"
	"nvr/pkg/video/gortsplib/pkg/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pion/rtp"
)

type rtpPacketMultiBuffer struct {
	count   uint64
	buffers []rtp.Packet
	cur     uint64
}

func newRTPPacketMultiBuffer(count uint64) *rtpPacketMultiBuffer {
	buffers := make([]rtp.Packet, count)
	return &rtpPacketMultiBuffer{
		count:   count,
		buffers: buffers,
	}
}

func (mb *rtpPacketMultiBuffer) next() *rtp.Packet {
	ret := &mb.buffers[mb.cur%mb.count]
	mb.cur++
	return ret
}

func stringsReverseIndex(s, substr string) int {
	for i := len(s) - 1 - len(substr); i >= 0; i-- {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Errors.
var (
	ErrTrackInvalid = errors.New("invalid track path")
	ErrPathInvalid  = errors.New(
		"path of a SETUP request must end with a slash." +
			" This typically happens when VLC fails a request," +
			" and then switches to an unsupported RTSP dialect")
	ErrTrackParseError = errors.New("unable to parse track id")
	ErrTrackPathError  = errors.New("can't setup tracks with different paths")
)

func setupGetTrackIDPathQuery(
	u *url.URL,
	thMode *headers.TransportMode,
	announcedTracks []*ServerSessionAnnouncedTrack,
	setuppedPath *string,
	setuppedQuery *string,
	setuppedBaseURL *url.URL,
) (int, string, string, error) {
	pathAndQuery, ok := u.RTSPPathAndQuery()
	if !ok {
		return 0, "", "", liberrors.ErrServerInvalidPath
	}

	if thMode != nil && *thMode != headers.TransportModePlay {
		for trackID, track := range announcedTracks {
			u2, _ := track.track.url(setuppedBaseURL)
			if u2.String() == u.String() {
				return trackID, *setuppedPath, *setuppedQuery, nil
			}
		}

		return 0, "", "", fmt.Errorf("%w (%s)", ErrTrackInvalid, pathAndQuery)
	}

	i := stringsReverseIndex(pathAndQuery, "/trackID=")

	// URL doesn't contain trackID - it's track zero
	if i < 0 {
		if !strings.HasSuffix(pathAndQuery, "/") {
			return 0, "", "", ErrPathInvalid
		}
		pathAndQuery = pathAndQuery[:len(pathAndQuery)-1]

		path, query := url.PathSplitQuery(pathAndQuery)

		// we assume it's track 0
		return 0, path, query, nil
	}

	tmp, err := strconv.ParseInt(pathAndQuery[i+len("/trackID="):], 10, 64)
	if err != nil || tmp < 0 {
		return 0, "", "", fmt.Errorf("%w (%v)", ErrTrackParseError, pathAndQuery)
	}
	trackID := int(tmp)
	pathAndQuery = pathAndQuery[:i]

	path, query := url.PathSplitQuery(pathAndQuery)

	if setuppedPath != nil && (path != *setuppedPath || query != *setuppedQuery) {
		return 0, "", "", ErrTrackPathError
	}

	return trackID, path, query, nil
}

// ServerSessionState is a state of a ServerSession.
type ServerSessionState int

// States.
const (
	ServerSessionStateInitial ServerSessionState = iota
	ServerSessionStatePrePlay
	ServerSessionStatePlay
	ServerSessionStatePreRecord
	ServerSessionStateRecord
)

// String implements fmt.Stringer.
func (s ServerSessionState) String() string {
	switch s {
	case ServerSessionStateInitial:
		return "initial"
	case ServerSessionStatePrePlay:
		return "prePlay"
	case ServerSessionStatePlay:
		return "play"
	case ServerSessionStatePreRecord:
		return "preRecord"
	case ServerSessionStateRecord:
		return "record"
	}
	return "unknown"
}

// ServerSessionSetuppedTrack is a setupped track of a ServerSession.
type ServerSessionSetuppedTrack struct {
	tcpChannel int
}

// ServerSessionAnnouncedTrack is an announced track of a ServerSession.
type ServerSessionAnnouncedTrack struct {
	track   Track
	cleaner *rtpcleaner.Cleaner
}

// ServerSession is a server-side RTSP session.
type ServerSession struct {
	s        *Server
	secretID string // must not be shared, allows to take ownership of the session
	author   *ServerConn

	ctx                context.Context
	ctxCancel          func()
	conns              map[*ServerConn]struct{}
	state              ServerSessionState
	setuppedTracks     map[int]*ServerSessionSetuppedTrack
	tcpTracksByChannel map[int]int
	IsTransportSetup   bool
	setuppedBaseURL    *url.URL      // publish
	setuppedStream     *ServerStream // read
	setuppedPath       *string
	setuppedQuery      *string
	lastRequestTime    time.Time
	tcpConn            *ServerConn
	announcedTracks    []*ServerSessionAnnouncedTrack // publish
	writerRunning      bool
	writeBuffer        *ringbuffer.RingBuffer

	// writer channels
	writerDone chan struct{}

	// in
	request     chan sessionRequestReq
	connRemove  chan *ServerConn
	startWriter chan struct{}
}

func newServerSession(
	s *Server,
	secretID string,
	author *ServerConn,
) *ServerSession {
	ctx, ctxCancel := context.WithCancel(s.ctx)

	ss := &ServerSession{
		s:               s,
		secretID:        secretID,
		author:          author,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
		conns:           make(map[*ServerConn]struct{}),
		lastRequestTime: time.Now(),
		request:         make(chan sessionRequestReq),
		connRemove:      make(chan *ServerConn),
		startWriter:     make(chan struct{}),
	}

	s.wg.Add(1)
	go ss.run()

	return ss
}

// Close closes the ServerSession.
func (ss *ServerSession) Close() error {
	ss.ctxCancel()
	return nil
}

// State returns the state of the session.
func (ss *ServerSession) State() ServerSessionState {
	return ss.state
}

// SetuppedTracks returns the setupped tracks.
func (ss *ServerSession) SetuppedTracks() map[int]*ServerSessionSetuppedTrack {
	return ss.setuppedTracks
}

// AnnouncedTracks returns the announced tracks.
func (ss *ServerSession) AnnouncedTracks() []*ServerSessionAnnouncedTrack {
	return ss.announcedTracks
}

func (ss *ServerSession) checkState(allowed map[ServerSessionState]struct{}) error {
	if _, ok := allowed[ss.state]; ok {
		return nil
	}

	allowedList := make([]fmt.Stringer, len(allowed))
	i := 0
	for a := range allowed {
		allowedList[i] = a
		i++
	}
	return liberrors.ServerInvalidStateError{AllowedList: allowedList, State: ss.state}
}

func (ss *ServerSession) run() {
	defer ss.s.wg.Done()

	if h, ok := ss.s.Handler.(ServerHandlerOnSessionOpen); ok {
		h.OnSessionOpen(&ServerHandlerOnSessionOpenCtx{
			Session: ss,
			Conn:    ss.author,
		})
	}

	err := ss.runInner()
	ss.ctxCancel()

	if ss.state == ServerSessionStatePlay {
		ss.setuppedStream.readerSetInactive(ss)
	}

	if ss.setuppedStream != nil {
		ss.setuppedStream.readerRemove(ss)
	}

	if ss.writerRunning {
		ss.writeBuffer.Close()
		<-ss.writerDone
	}

	for sc := range ss.conns {
		if sc == ss.tcpConn {
			sc.Close()

			// make sure that OnFrame() is never called after OnSessionClose()
			<-sc.done
		}

		select {
		case sc.sessionRemove <- ss:
		case <-sc.ctx.Done():
		}
	}

	select {
	case ss.s.sessionClose <- ss:
	case <-ss.s.ctx.Done():
	}

	if h, ok := ss.s.Handler.(ServerHandlerOnSessionClose); ok {
		h.OnSessionClose(&ServerHandlerOnSessionCloseCtx{
			Session: ss,
			Error:   err,
		})
	}
}

func (ss *ServerSession) runInner() error { //nolint:funlen,gocognit
	for {
		select {
		case req := <-ss.request:
			ss.lastRequestTime = time.Now()

			if _, ok := ss.conns[req.sc]; !ok {
				ss.conns[req.sc] = struct{}{}
			}

			res, err := ss.handleRequest(req.sc, req.req)

			returnedSession := ss

			if err == nil || errors.Is(err, errSwitchReadFunc) {
				// ANNOUNCE responses don't contain the session header.
				if req.req.Method != base.Announce &&
					req.req.Method != base.Teardown {
					if res.Header == nil {
						res.Header = make(base.Header)
					}

					res.Header["Session"] = headers.Session{
						Session: ss.secretID,
					}.Marshal()
				}

				// after a TEARDOWN, session must be unpaired with the connection.
				if req.req.Method == base.Teardown {
					returnedSession = nil
				}
			}

			savedMethod := req.req.Method

			req.res <- sessionRequestRes{
				res: res,
				err: err,
				ss:  returnedSession,
			}

			if (err == nil || errors.Is(err, errSwitchReadFunc)) && savedMethod == base.Teardown {
				return liberrors.ServerSessionTeardownError{Author: req.sc.NetConn().RemoteAddr()}
			}

		case sc := <-ss.connRemove:
			delete(ss.conns, sc)

			if len(ss.conns) == 0 {
				return liberrors.ErrServerSessionNotInUse
			}

		case <-ss.startWriter:
			if !ss.writerRunning && (ss.state == ServerSessionStateRecord ||
				ss.state == ServerSessionStatePlay) &&
				ss.IsTransportSetup {
				ss.writerRunning = true
				ss.writerDone = make(chan struct{})
				go ss.runWriter()
			}

		case <-ss.ctx.Done():
			return liberrors.ErrServerTerminated
		}
	}
}

func (ss *ServerSession) handleRequest(sc *ServerConn, req *base.Request) (*base.Response, error) { //nolint:funlen
	if ss.tcpConn != nil && sc != ss.tcpConn {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerSessionLinkedToOtherConn
	}

	switch req.Method {
	case base.Options:
		return ss.handleOptions(sc)

	case base.Announce:
		return ss.handleAnnounce(sc, req)

	case base.Setup:
		return ss.handleSetup(sc, req)

	case base.Play:
		return ss.handlePlay(sc, req)

	case base.Record:
		return ss.handleRecord(sc, req)

	case base.Pause:
		return ss.handlePause(sc, req)

	case base.Teardown:
		var err error
		if ss.state == ServerSessionStatePlay || ss.state == ServerSessionStateRecord {
			ss.tcpConn.readFunc = ss.tcpConn.readFuncStandard
			err = errSwitchReadFunc
		}

		return &base.Response{
			StatusCode: base.StatusOK,
		}, err

	case base.GetParameter:
		if h, ok := sc.s.Handler.(ServerHandlerOnGetParameter); ok {
			pathAndQuery, ok := req.URL.RTSPPathAndQuery()
			if !ok {
				return &base.Response{
					StatusCode: base.StatusBadRequest,
				}, liberrors.ErrServerInvalidPath
			}

			path, query := url.PathSplitQuery(pathAndQuery)

			return h.OnGetParameter(&ServerHandlerOnGetParameterCtx{
				Session: ss,
				Conn:    sc,
				Request: req,
				Path:    path,
				Query:   query,
			})
		}

		// GET_PARAMETER is used like a ping when reading, and sometimes
		// also when publishing; reply with 200
		return &base.Response{
			StatusCode: base.StatusOK,
			Header: base.Header{
				"Content-Type": base.HeaderValue{"text/parameters"},
			},
			Body: []byte{},
		}, nil
	}

	return &base.Response{
		StatusCode: base.StatusBadRequest,
	}, liberrors.ServerUnhandledRequestError{Request: req}
}

func (ss *ServerSession) handleOptions(sc *ServerConn) (*base.Response, error) {
	var methods []string
	if _, ok := sc.s.Handler.(ServerHandlerOnDescribe); ok {
		methods = append(methods, string(base.Describe))
	}
	if _, ok := sc.s.Handler.(ServerHandlerOnAnnounce); ok {
		methods = append(methods, string(base.Announce))
	}
	if _, ok := sc.s.Handler.(ServerHandlerOnSetup); ok {
		methods = append(methods, string(base.Setup))
	}
	if _, ok := sc.s.Handler.(ServerHandlerOnPlay); ok {
		methods = append(methods, string(base.Play))
	}
	if _, ok := sc.s.Handler.(ServerHandlerOnRecord); ok {
		methods = append(methods, string(base.Record))
	}
	if _, ok := sc.s.Handler.(ServerHandlerOnPause); ok {
		methods = append(methods, string(base.Pause))
	}
	methods = append(methods, string(base.GetParameter))
	if _, ok := sc.s.Handler.(ServerHandlerOnSetParameter); ok {
		methods = append(methods, string(base.SetParameter))
	}
	methods = append(methods, string(base.Teardown))

	return &base.Response{
		StatusCode: base.StatusOK,
		Header: base.Header{
			"Public": base.HeaderValue{strings.Join(methods, ", ")},
		},
	}, nil
}

// Errors.
var (
	ErrTrackGenURL      = errors.New("unable to generate track URL")
	ErrTrackInvalidURL  = errors.New("invalid track URL")
	ErrTrackInvalidPath = errors.New("invalid track path")
)

func (ss *ServerSession) handleAnnounce(sc *ServerConn, req *base.Request) (*base.Response, error) { //nolint:funlen
	err := ss.checkState(map[ServerSessionState]struct{}{
		ServerSessionStateInitial: {},
	})
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	pathAndQuery, ok := req.URL.RTSPPathAndQuery()
	if !ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerInvalidPath
	}

	path, query := url.PathSplitQuery(pathAndQuery)

	ct, ok := req.Header["Content-Type"]
	if !ok || len(ct) != 1 {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerContentTypeMissing
	}

	if ct[0] != "application/sdp" {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerContentTypeUnsupportedError{CT: ct}
	}

	var tracks Tracks
	_, err = tracks.Unmarshal(req.Body, false)
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ServerSDPinvalidError{Err: err}
	}

	for _, track := range tracks {
		trackURL, err := track.url(req.URL)
		if err != nil {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, ErrTrackGenURL
		}

		trackPath, ok := trackURL.RTSPPathAndQuery()
		if !ok {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, fmt.Errorf("%w (%v)", ErrTrackInvalidURL, trackURL)
		}

		if !strings.HasPrefix(trackPath, path) {
			return &base.Response{
					StatusCode: base.StatusBadRequest,
				}, fmt.Errorf("%w: must begin with '%s', but is '%s'",
					ErrTrackInvalidPath, path, trackPath)
		}
	}

	res, err := ss.s.Handler.(ServerHandlerOnAnnounce).OnAnnounce(&ServerHandlerOnAnnounceCtx{
		Server:  ss.s,
		Session: ss,
		Conn:    sc,
		Request: req,
		Path:    path,
		Query:   query,
		Tracks:  tracks,
	})

	if res.StatusCode != base.StatusOK {
		return res, err
	}

	ss.state = ServerSessionStatePreRecord
	ss.setuppedPath = &path
	ss.setuppedQuery = &query
	ss.setuppedBaseURL = req.URL

	ss.announcedTracks = make([]*ServerSessionAnnouncedTrack, len(tracks))
	for trackID, track := range tracks {
		ss.announcedTracks[trackID] = &ServerSessionAnnouncedTrack{
			track: track,
		}
	}

	return res, err
}

func (ss *ServerSession) handleSetup(sc *ServerConn, //nolint:funlen,gocognit
	req *base.Request,
) (*base.Response, error) {
	err := ss.checkState(map[ServerSessionState]struct{}{
		ServerSessionStateInitial:   {},
		ServerSessionStatePrePlay:   {},
		ServerSessionStatePreRecord: {},
	})
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	var inTH headers.Transport
	err = inTH.Unmarshal(req.Header["Transport"])
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ServerTransportHeaderInvalidError{Err: err}
	}

	if inTH.Protocol != headers.TransportProtocolTCP {
		return &base.Response{
			StatusCode: base.StatusUnsupportedTransport,
		}, nil
	}

	trackID, path, query, err := setupGetTrackIDPathQuery(req.URL, inTH.Mode,
		ss.announcedTracks, ss.setuppedPath, ss.setuppedQuery, ss.setuppedBaseURL)
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	if _, ok := ss.setuppedTracks[trackID]; ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ServerTrackAlreadySetupError{TrackID: trackID}
	}

	if inTH.InterleavedIDs == nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerTransportHeaderNoInterleavedIDs
	}

	if (inTH.InterleavedIDs[0]%2) != 0 ||
		(inTH.InterleavedIDs[0]+1) != inTH.InterleavedIDs[1] {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerTransportHeaderInvalidInterleavedIDs
	}

	if _, ok := ss.tcpTracksByChannel[inTH.InterleavedIDs[0]]; ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerTransportHeaderInterleavedIDsAlreadyUsed
	}

	switch ss.state {
	case ServerSessionStateInitial, ServerSessionStatePrePlay: // play
		if inTH.Mode != nil && *inTH.Mode != headers.TransportModePlay {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, liberrors.ServerTransportHeaderInvalidModeError{Mode: inTH.Mode}
		}

	default: // record

		if inTH.Mode == nil || *inTH.Mode != headers.TransportModeRecord {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, liberrors.ServerTransportHeaderInvalidModeError{Mode: inTH.Mode}
		}
	}

	res, stream, err := ss.s.Handler.(ServerHandlerOnSetup).OnSetup(&ServerHandlerOnSetupCtx{ //nolint:forcetypeassert
		Server:  ss.s,
		Session: ss,
		Conn:    sc,
		Request: req,
		Path:    path,
		Query:   query,
		TrackID: trackID,
	})

	// workaround to prevent a bug in rtspclientsink
	// that makes impossible for the client to receive the response
	// and send frames.
	// this was causing problems during unit tests.
	if ua, ok := req.Header["User-Agent"]; ok && len(ua) == 1 &&
		strings.HasPrefix(ua[0], "GStreamer") {
		select {
		case <-time.After(1 * time.Second):
		case <-ss.ctx.Done():
		}
	}

	if res.StatusCode != base.StatusOK {
		return res, err
	}

	if ss.state == ServerSessionStateInitial {
		if err := stream.readerAdd(ss); err != nil {
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, err
		}

		ss.state = ServerSessionStatePrePlay
		ss.setuppedPath = &path
		ss.setuppedQuery = &query
		ss.setuppedStream = stream
	}

	th := headers.Transport{}

	if ss.state == ServerSessionStatePrePlay {
		ssrc := stream.ssrc(trackID)
		if ssrc != 0 {
			th.SSRC = &ssrc
		}
	}

	ss.IsTransportSetup = true

	if res.Header == nil {
		res.Header = make(base.Header)
	}

	sst := &ServerSessionSetuppedTrack{}

	if ss.tcpTracksByChannel == nil {
		ss.tcpTracksByChannel = make(map[int]int)
	}

	ss.tcpTracksByChannel[inTH.InterleavedIDs[0]] = trackID

	th.Protocol = headers.TransportProtocolTCP
	th.InterleavedIDs = inTH.InterleavedIDs

	if ss.setuppedTracks == nil {
		ss.setuppedTracks = make(map[int]*ServerSessionSetuppedTrack)
	}

	ss.setuppedTracks[trackID] = sst

	res.Header["Transport"] = th.Marshal()

	return res, err
}

func (ss *ServerSession) handlePlay(sc *ServerConn, req *base.Request) (*base.Response, error) { //nolint:funlen
	// play can be sent twice, allow calling it even if we're already playing
	err := ss.checkState(map[ServerSessionState]struct{}{
		ServerSessionStatePrePlay: {},
		ServerSessionStatePlay:    {},
	})
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	pathAndQuery, ok := req.URL.RTSPPathAndQuery()
	if !ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerInvalidPath
	}

	// path can end with a slash due to Content-Base, remove it
	pathAndQuery = strings.TrimSuffix(pathAndQuery, "/")

	path, query := url.PathSplitQuery(pathAndQuery)

	if ss.State() == ServerSessionStatePrePlay &&
		path != *ss.setuppedPath {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ServerPathHasChangedError{Prev: *ss.setuppedPath, Cur: path}
	}

	// allocate writeBuffer before calling OnPlay().
	// in this way it's possible to call ServerSession.WritePacket*()
	// inside the callback.
	if ss.state != ServerSessionStatePlay {
		ss.writeBuffer, _ = ringbuffer.New(uint64(ss.s.WriteBufferCount))
	}

	res, err := sc.s.Handler.(ServerHandlerOnPlay).OnPlay(&ServerHandlerOnPlayCtx{
		Session: ss,
		Conn:    sc,
		Request: req,
		Path:    path,
		Query:   query,
	})

	if res.StatusCode != base.StatusOK {
		if ss.State() == ServerSessionStatePrePlay {
			ss.writeBuffer = nil
		}
		return res, err
	}

	if ss.state == ServerSessionStatePlay {
		return res, err
	}

	ss.state = ServerSessionStatePlay

	ss.tcpConn = sc
	ss.tcpConn.readFunc = ss.tcpConn.readFuncTCP
	err = errSwitchReadFunc

	ss.writeBuffer, _ = ringbuffer.New(uint64(ss.s.ReadBufferCount))
	// runWriter() is called by ServerConn after the response has been sent

	ss.setuppedStream.readerSetActive(ss)

	var trackIDs []int
	for trackID := range ss.setuppedTracks {
		trackIDs = append(trackIDs, trackID)
	}

	sort.Slice(trackIDs, func(a, b int) bool {
		return trackIDs[a] < trackIDs[b]
	})

	var ri headers.RTPinfo
	now := time.Now()

	for _, trackID := range trackIDs {
		seqNum, ts, ok := ss.setuppedStream.rtpInfo(trackID, now)
		if !ok {
			continue
		}

		u := &url.URL{
			Scheme: req.URL.Scheme,
			User:   req.URL.User,
			Host:   req.URL.Host,
			Path:   "/" + *ss.setuppedPath + "/trackID=" + strconv.FormatInt(int64(trackID), 10),
		}

		ri = append(ri, &headers.RTPInfoEntry{
			URL:            u.String(),
			SequenceNumber: &seqNum,
			Timestamp:      &ts,
		})
	}
	if len(ri) > 0 {
		if res.Header == nil {
			res.Header = make(base.Header)
		}
		res.Header["RTP-Info"] = ri.Marshal()
	}

	return res, err
}

func (ss *ServerSession) handleRecord(sc *ServerConn, req *base.Request) (*base.Response, error) { //nolint:funlen
	err := ss.checkState(map[ServerSessionState]struct{}{
		ServerSessionStatePreRecord: {},
	})
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	if len(ss.setuppedTracks) != len(ss.announcedTracks) {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerNotAllAnnouncedTracksSetup
	}

	pathAndQuery, ok := req.URL.RTSPPathAndQuery()
	if !ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerInvalidPath
	}

	// path can end with a slash due to Content-Base, remove it
	pathAndQuery = strings.TrimSuffix(pathAndQuery, "/")

	path, query := url.PathSplitQuery(pathAndQuery)

	if path != *ss.setuppedPath {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ServerPathHasChangedError{Prev: *ss.setuppedPath, Cur: path}
	}

	// allocate writeBuffer before calling OnRecord().
	// in this way it's possible to call ServerSession.WritePacket*()
	// inside the callback.
	ss.writeBuffer, _ = ringbuffer.New(uint64(8))

	res, err := ss.s.Handler.(ServerHandlerOnRecord).OnRecord(&ServerHandlerOnRecordCtx{
		Session: ss,
		Conn:    sc,
		Request: req,
		Path:    path,
		Query:   query,
	})

	if res.StatusCode != base.StatusOK {
		ss.writeBuffer = nil
		return res, err
	}

	ss.state = ServerSessionStateRecord

	for _, at := range ss.announcedTracks {
		_, isH264 := at.track.(*TrackH264)
		at.cleaner = rtpcleaner.New(isH264)
	}

	ss.tcpConn = sc
	ss.tcpConn.readFunc = ss.tcpConn.readFuncTCP
	err = errSwitchReadFunc

	// runWriter() is called by conn after sending the response
	return res, err
}

func (ss *ServerSession) handlePause(sc *ServerConn, req *base.Request) (*base.Response, error) { //nolint:funlen
	err := ss.checkState(map[ServerSessionState]struct{}{
		ServerSessionStatePrePlay:   {},
		ServerSessionStatePlay:      {},
		ServerSessionStatePreRecord: {},
		ServerSessionStateRecord:    {},
	})
	if err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	pathAndQuery, ok := req.URL.RTSPPathAndQuery()
	if !ok {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, liberrors.ErrServerInvalidPath
	}

	// path can end with a slash due to Content-Base, remove it
	pathAndQuery = strings.TrimSuffix(pathAndQuery, "/")

	path, query := url.PathSplitQuery(pathAndQuery)

	res, err := ss.s.Handler.(ServerHandlerOnPause).OnPause(&ServerHandlerOnPauseCtx{
		Session: ss,
		Conn:    sc,
		Request: req,
		Path:    path,
		Query:   query,
	})

	if res.StatusCode != base.StatusOK {
		return res, err
	}

	if ss.writerRunning {
		ss.writeBuffer.Close()
		<-ss.writerDone
		ss.writerRunning = false
	}

	switch ss.state {
	case ServerSessionStatePlay:
		ss.setuppedStream.readerSetInactive(ss)

		ss.state = ServerSessionStatePrePlay

		ss.tcpConn.readFunc = ss.tcpConn.readFuncStandard
		err = errSwitchReadFunc

		ss.tcpConn = nil

	case ServerSessionStateRecord:
		ss.tcpConn.readFunc = ss.tcpConn.readFuncStandard
		err = errSwitchReadFunc

		err := ss.tcpConn.conn.SetReadDeadline(time.Time{})
		if err != nil {
			return nil, err
		}
		ss.tcpConn = nil

		for _, at := range ss.announcedTracks {
			at.cleaner = nil
		}

		ss.state = ServerSessionStatePreRecord
	}

	return res, err
}

func (ss *ServerSession) runWriter() {
	defer close(ss.writerDone)

	rtpFrames := make(map[int]*base.InterleavedFrame, len(ss.setuppedTracks))

	for trackID, sst := range ss.setuppedTracks {
		rtpFrames[trackID] = &base.InterleavedFrame{Channel: sst.tcpChannel}
	}

	buf := make([]byte, maxPacketSize+4)

	writeFunc := func(trackID int, payload []byte) {
		f := rtpFrames[trackID]
		f.Payload = payload
		n, _ := f.MarshalTo(buf)

		ss.tcpConn.conn.SetWriteDeadline(time.Now().Add(ss.s.WriteTimeout)) //nolint:errcheck
		ss.tcpConn.conn.Write(buf[:n])                                      //nolint:errcheck
	}

	for {
		tmp, ok := ss.writeBuffer.Pull()
		if !ok {
			return
		}
		data := tmp.(trackTypePayload) //nolint:forcetypeassert

		writeFunc(data.trackID, data.payload)
	}
}

func (ss *ServerSession) writePacketRTP(trackID int, byts []byte) {
	if _, ok := ss.setuppedTracks[trackID]; !ok {
		return
	}

	ss.writeBuffer.Push(trackTypePayload{
		trackID: trackID,
		payload: byts,
	})
}

// WritePacketRTP writes a RTP packet to the session.
func (ss *ServerSession) WritePacketRTP(trackID int, pkt *rtp.Packet) {
	byts, err := pkt.Marshal()
	if err != nil {
		return
	}

	ss.writePacketRTP(trackID, byts)
}
