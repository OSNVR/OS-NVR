package hls

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"nvr/pkg/video/gortsplib"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SegmentOrGap .
type SegmentOrGap interface {
	getRenderedDuration() time.Duration
}

// Gap .
type Gap struct {
	renderedDuration time.Duration
}

func (g Gap) getRenderedDuration() time.Duration {
	return g.renderedDuration
}

func targetDuration(segments []SegmentOrGap) uint {
	ret := uint(0)

	// EXTINF, when rounded to the nearest integer, must be <= EXT-X-TARGETDURATION
	for _, sog := range segments {
		v := uint(math.Round(sog.getRenderedDuration().Seconds()))
		if v > ret {
			ret = v
		}
	}

	return ret
}

func partTargetDuration(
	segments []SegmentOrGap,
	nextSegmentParts []*MuxerPartFinalized,
) time.Duration {
	var ret time.Duration

	for _, sog := range segments {
		seg, ok := sog.(*SegmentFinalized)
		if !ok {
			continue
		}

		for _, part := range seg.Parts {
			if part.renderedDuration > ret {
				ret = part.renderedDuration
			}
		}
	}

	for _, part := range nextSegmentParts {
		if part.renderedDuration > ret {
			ret = part.renderedDuration
		}
	}

	return ret
}

type playlist struct {
	mu        sync.Mutex
	cancelled bool

	muxerID      uint16
	segmentCount int

	segments           []SegmentOrGap
	segmentsByName     map[string]*SegmentFinalized
	segmentDeleteCount int
	partsByName        map[string]*MuxerPartFinalized
	nextSegmentID      uint64
	nextSegmentParts   []*MuxerPartFinalized
	nextPartID         uint64

	playlistsOnHold    map[blockingPlaylistRequest]struct{}
	partsOnHold        map[blockingPartRequest]struct{}
	segFinalOnHold     map[chan struct{}]struct{}
	partFinalOnHold    map[chan struct{}]struct{}
	nextSegmentsOnHold map[nextSegmentRequest]struct{}
}

func newPlaylist(muxerID uint16, segmentCount int) *playlist {
	return &playlist{
		mu:             sync.Mutex{},
		muxerID:        muxerID,
		segmentCount:   segmentCount,
		segmentsByName: make(map[string]*SegmentFinalized),
		partsByName:    make(map[string]*MuxerPartFinalized),

		playlistsOnHold:    make(map[blockingPlaylistRequest]struct{}),
		partsOnHold:        make(map[blockingPartRequest]struct{}),
		segFinalOnHold:     make(map[chan struct{}]struct{}),
		partFinalOnHold:    make(map[chan struct{}]struct{}),
		nextSegmentsOnHold: make(map[nextSegmentRequest]struct{}),
	}
}

func (p *playlist) cancel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return
	}

	for req := range p.playlistsOnHold {
		close(req.res)
	}
	for req := range p.partsOnHold {
		close(req.res)
	}
	for done := range p.segFinalOnHold {
		close(done)
	}
	for done := range p.partFinalOnHold {
		close(done)
	}
	for req := range p.nextSegmentsOnHold {
		close(req.res)
	}
	p.cancelled = true
}

func (p *playlist) checkPending() {
	if p.hasContent() {
		for req := range p.playlistsOnHold {
			if !p.hasPart(req.msnint, req.partint) {
				return
			}
			req.res <- MuxerFileResponse{
				Status: http.StatusOK,
				Header: map[string]string{
					"Content-Type": `audio/mpegURL`,
				},
				Body: bytes.NewReader(p.fullPlaylist(req.isDeltaUpdate)),
			}
			delete(p.playlistsOnHold, req)
		}
	}
	for req := range p.partsOnHold {
		if p.nextPartID <= req.partID {
			return
		}
		part := p.partsByName[req.partName]
		req.res <- MuxerFileResponse{
			Status: http.StatusOK,
			Header: map[string]string{
				"Content-Type": "video/mp4",
			},
			Body: part.reader(),
		}
		delete(p.partsOnHold, req)
	}
}

func (p *playlist) hasContent() bool {
	return len(p.segments) >= 1
}

func (p *playlist) hasPart(segmentID uint64, partID uint64) bool {
	if !p.hasContent() {
		return false
	}

	for _, sop := range p.segments {
		seg, ok := sop.(*SegmentFinalized)
		if !ok {
			continue
		}

		if segmentID != seg.ID {
			continue
		}

		// If the Client requests a Part Index greater than that of the final
		// Partial Segment of the Parent Segment, the Server MUST treat the
		// request as one for Part Index 0 of the following Parent Segment.
		if partID >= uint64(len(seg.Parts)) {
			segmentID++
			partID = 0
			continue
		}

		return true
	}

	if segmentID != p.nextSegmentID {
		return false
	}

	if partID >= uint64(len(p.nextSegmentParts)) {
		return false
	}

	return true
}

func (p *playlist) file(name, msn, part, skip string) MuxerFileResponse {
	switch {
	case name == "stream.m3u8":
		return p.playlistReader(msn, part, skip)

	case strings.HasSuffix(name, ".mp4"):
		return p.segmentReader(name)

	// Apple bug?
	case strings.HasSuffix(name, ".mp"):
		return p.segmentReader(name + "4")

	default:
		return MuxerFileResponse{Status: http.StatusNotFound}
	}
}

type blockingPlaylistRequest struct {
	isDeltaUpdate bool
	msnint        uint64
	partint       uint64
	res           chan MuxerFileResponse
}

func (p *playlist) blockingPlaylist(isDeltaUpdate bool, msnint uint64, partint uint64) MuxerFileResponse {
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return MuxerFileReponseCancelled()
	}

	// If the _HLS_msn is greater than the Media Sequence Number of the last
	// Media Segment in the current Playlist plus two, or if the _HLS_part
	// exceeds the last Partial Segment in the current Playlist by the
	// Advance Part Limit, then the server SHOULD immediately return Bad
	// Request, such as HTTP 400.
	if msnint > (p.nextSegmentID + 1) {
		p.mu.Unlock()
		return MuxerFileResponse{Status: http.StatusBadRequest}
	}

	if p.hasContent() && p.hasPart(msnint, partint) {
		res := MuxerFileResponse{
			Status: http.StatusOK,
			Header: map[string]string{
				"Content-Type": `audio/mpegURL`,
			},
			Body: bytes.NewReader(p.fullPlaylist(isDeltaUpdate)),
		}
		p.mu.Unlock()
		return res
	}

	res := make(chan MuxerFileResponse)
	req := blockingPlaylistRequest{
		isDeltaUpdate: isDeltaUpdate,
		msnint:        msnint,
		partint:       partint,
		res:           res,
	}

	p.playlistsOnHold[req] = struct{}{}
	p.mu.Unlock() // Must be unlocked here.

	res2, ok := <-res
	if !ok {
		return MuxerFileReponseCancelled()
	}
	return res2
}

func (p *playlist) playlistReader(msn, part, skip string) MuxerFileResponse {
	isDeltaUpdate := skip == "YES" || skip == "v2"

	var msnint uint64
	if msn != "" {
		var err error
		msnint, err = strconv.ParseUint(msn, 10, 64)
		if err != nil {
			return MuxerFileResponse{Status: http.StatusBadRequest}
		}
	}

	var partint uint64
	if part != "" {
		var err error
		partint, err = strconv.ParseUint(part, 10, 64)
		if err != nil {
			return MuxerFileResponse{Status: http.StatusBadRequest}
		}
	}

	if msn != "" {
		return p.blockingPlaylist(isDeltaUpdate, msnint, partint)
	}

	// part without msn is not supported.
	if part != "" {
		return MuxerFileResponse{Status: http.StatusBadRequest}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cancelled {
		return MuxerFileReponseCancelled()
	}

	if !p.hasContent() {
		return MuxerFileResponse{Status: http.StatusNotFound}
	}
	return MuxerFileResponse{
		Status: http.StatusOK,
		Header: map[string]string{
			"Content-Type": `audio/mpegURL`,
		},
		Body: bytes.NewReader(p.fullPlaylist(isDeltaUpdate)),
	}
}

func primaryPlaylist(
	videoTrack *gortsplib.TrackH264,
	audioTrack *gortsplib.TrackMPEG4Audio,
) MuxerFileResponse {
	return MuxerFileResponse{
		Status: http.StatusOK,
		Header: map[string]string{
			"Content-Type": `audio/mpegURL`,
		},
		Body: func() io.Reader {
			var codecs []string

			sps := videoTrack.SPS
			if len(sps) >= 4 {
				codecs = append(codecs, "avc1."+hex.EncodeToString(sps[1:4]))
			}

			// https://developer.mozilla.org/en-US/docs/Web/Media/Formats/codecs_parameter
			if audioTrack != nil {
				codecs = append(
					codecs,
					"mp4a.40."+strconv.FormatInt(int64(audioTrack.Config.Type), 10),
				)
			}

			return bytes.NewReader([]byte("#EXTM3U\n" +
				"#EXT-X-VERSION:9\n" +
				"#EXT-X-INDEPENDENT-SEGMENTS\n" +
				"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=200000,CODECS=\"" + strings.Join(codecs, ",") + "\"\n" +
				"stream.m3u8\n"))
		}(),
	}
}

func (p *playlist) fullPlaylist(isDeltaUpdate bool) []byte { //nolint:funlen
	cnt := "#EXTM3U\n"
	cnt += "#EXT-X-VERSION:9\n"

	targetDuration := targetDuration(p.segments)
	cnt += "#EXT-X-TARGETDURATION:" + strconv.FormatUint(uint64(targetDuration), 10) + "\n"

	skipBoundary := float64(targetDuration * 6)

	partTargetDuration := partTargetDuration(p.segments, p.nextSegmentParts)

	// The value is an enumerated-string whose value is YES if the server
	// supports Blocking Playlist Reload
	cnt += "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES"

	// The value is a decimal-floating-point number of seconds that
	// indicates the server-recommended minimum distance from the end of
	// the Playlist at which clients should begin to play or to which
	// they should seek when playing in Low-Latency Mode.  Its value MUST
	// be at least twice the Part Target Duration.  Its value SHOULD be
	// at least three times the Part Target Duration.
	cnt += ",PART-HOLD-BACK=" + strconv.FormatFloat((partTargetDuration).Seconds()*2.5, 'f', 5, 64)

	// Indicates that the Server can produce Playlist Delta Updates in
	// response to the _HLS_skip Delivery Directive.  Its value is the
	// Skip Boundary, a decimal-floating-point number of seconds.  The
	// Skip Boundary MUST be at least six times the Target Duration.
	cnt += ",CAN-SKIP-UNTIL=" + strconv.FormatFloat(skipBoundary, 'f', -1, 64)

	cnt += "\n"

	cnt += "#EXT-X-PART-INF:PART-TARGET=" + strconv.FormatFloat(partTargetDuration.Seconds(), 'f', -1, 64) + "\n"

	cnt += "#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(int64(p.segmentDeleteCount), 10) + "\n"

	skipped := 0
	if !isDeltaUpdate {
		cnt += "#EXT-X-MAP:URI=\"init.mp4\"\n"
	} else {
		var curDuration time.Duration
		shown := 0
		for _, segment := range p.segments {
			curDuration += segment.getRenderedDuration()
			if curDuration.Seconds() >= skipBoundary {
				break
			}
			shown++
		}
		skipped = len(p.segments) - shown
		cnt += "#EXT-X-SKIP:SKIPPED-SEGMENTS=" + strconv.FormatInt(int64(skipped), 10) + "\n"
	}

	for i, sog := range p.segments {
		if i < skipped {
			continue
		}

		switch seg := sog.(type) {
		case *SegmentFinalized:
			if (len(p.segments) - i) <= 2 {
				cnt += "#EXT-X-PROGRAM-DATE-TIME:" + seg.StartTime.Format("2006-01-02T15:04:05.999Z07:00") + "\n"
			}

			if (len(p.segments) - i) <= 2 {
				for _, part := range seg.Parts {
					cnt += "#EXT-X-PART:DURATION=" + strconv.FormatFloat(part.renderedDuration.Seconds(), 'f', 5, 64) +
						",URI=\"" + part.name() + ".mp4\""
					if part.isIndependent {
						cnt += ",INDEPENDENT=YES"
					}
					cnt += "\n"
				}
			}

			cnt += "#EXTINF:" + strconv.FormatFloat(seg.RenderedDuration.Seconds(), 'f', 5, 64) + ",\n" +
				seg.name + ".mp4\n"

		case *Gap:
			cnt += "#EXT-X-GAP\n" +
				"#EXTINF:" + strconv.FormatFloat(seg.renderedDuration.Seconds(), 'f', 5, 64) + ",\n" +
				"gap.mp4\n"
		}
	}

	for _, part := range p.nextSegmentParts {
		cnt += "#EXT-X-PART:DURATION=" + strconv.FormatFloat(part.renderedDuration.Seconds(), 'f', 5, 64) +
			",URI=\"" + part.name() + ".mp4\""
		if part.isIndependent {
			cnt += ",INDEPENDENT=YES"
		}
		cnt += "\n"
	}

	// preload hint must always be present
	// otherwise hls.js goes into a loop
	cnt += "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"" + partName(p.nextPartID) + ".mp4\"\n"

	return []byte(cnt)
}

type blockingPartRequest struct {
	partName string
	partID   uint64
	res      chan MuxerFileResponse
}

func (p *playlist) blockingPart(name string) MuxerFileResponse {
	res := make(chan MuxerFileResponse)
	req := blockingPartRequest{
		partName: name,
		res:      res,
	}

	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return MuxerFileReponseCancelled()
	}

	base := strings.TrimSuffix(req.partName, ".mp4")
	part, exist := p.partsByName[base]
	if exist {
		res := MuxerFileResponse{
			Status: http.StatusOK,
			Header: map[string]string{
				"Content-Type": "video/mp4",
			},
			Body: part.reader(),
		}
		p.mu.Unlock()
		return res
	}

	if req.partName != partName(p.nextPartID) {
		p.mu.Unlock()
		return MuxerFileResponse{Status: http.StatusNotFound}
	}

	req.partID = p.nextPartID
	p.partsOnHold[req] = struct{}{}

	p.mu.Unlock() // Must be unlocked here.
	res2, ok := <-res
	if !ok {
		return MuxerFileReponseCancelled()
	}
	return res2
}

func (p *playlist) segmentReader(fname string) MuxerFileResponse {
	switch {
	case strings.HasPrefix(fname, "seg"):
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cancelled {
			return MuxerFileReponseCancelled()
		}

		base := strings.TrimSuffix(fname, ".mp4")
		segment, exist := p.segmentsByName[base]
		if !exist {
			return MuxerFileResponse{Status: http.StatusNotFound}
		}
		return MuxerFileResponse{
			Status: http.StatusOK,
			Header: map[string]string{
				"Content-Type": "video/mp4",
			},
			Body: segment.reader(),
		}

	case strings.HasPrefix(fname, "part"):
		return p.blockingPart(fname)

	default:
		return MuxerFileResponse{Status: http.StatusNotFound}
	}
}

func (p *playlist) onSegmentFinalized(segment *SegmentFinalized) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return
	}

	// add initial gaps, required by iOS.
	if len(p.segments) == 0 {
		for i := 0; i < 7; i++ {
			p.segments = append(p.segments, &Gap{
				renderedDuration: segment.RenderedDuration,
			})
		}
	}

	p.segmentsByName[segment.name] = segment
	p.segments = append(p.segments, segment)
	p.nextSegmentID = segment.ID + 1
	p.nextSegmentParts = p.nextSegmentParts[:0]

	if len(p.segments) > p.segmentCount {
		toDelete := p.segments[0]

		if toDeleteSeg, ok := toDelete.(*SegmentFinalized); ok {
			for _, part := range toDeleteSeg.Parts {
				delete(p.partsByName, part.name())
			}

			delete(p.segmentsByName, toDeleteSeg.name)
		}

		p.segments[0] = nil // Free memory!
		p.segments = p.segments[1:]
		p.segmentDeleteCount++
	}

	for done := range p.segFinalOnHold {
		close(done)
		delete(p.segFinalOnHold, done)
	}
	for req := range p.nextSegmentsOnHold {
		if segment.ID > req.prevID {
			req.res <- segment
			delete(p.nextSegmentsOnHold, req)
		}
	}

	p.checkPending()
}

func (p *playlist) partFinalized(part *MuxerPartFinalized) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancelled {
		return
	}

	p.partsByName[part.name()] = part
	p.nextSegmentParts = append(p.nextSegmentParts, part)
	p.nextPartID = part.id + 1

	for done := range p.partFinalOnHold {
		close(done)
		delete(p.partFinalOnHold, done)
	}

	p.checkPending()
}

func (p *playlist) waitForSegFinalized() {
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return
	}

	res := make(chan struct{})
	p.segFinalOnHold[res] = struct{}{}
	p.mu.Unlock() // Must be unlocked here.
	<-res
}

func (p *playlist) waitForPartFinalized() {
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return
	}

	res := make(chan struct{})
	p.partFinalOnHold[res] = struct{}{}
	p.mu.Unlock() // Must be unlocked here.
	<-res
}

type nextSegmentRequest struct {
	prevID uint64
	res    chan *SegmentFinalized
}

func (p *playlist) nextSegment(maybePrevSeg *SegmentFinalized) (*SegmentFinalized, error) {
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		return nil, context.Canceled
	}

	prevID := func() uint64 {
		if maybePrevSeg == nil {
			return 0
		}
		prevSeg := maybePrevSeg
		if prevSeg.muxerID == p.muxerID && prevSeg.ID < p.nextSegmentID {
			return prevSeg.ID
		}
		return 0
	}()

	seg := func() *SegmentFinalized {
		for _, s := range p.segments {
			seg, ok := s.(*SegmentFinalized)
			if !ok {
				continue
			}
			if prevID < seg.ID {
				return seg
			}
		}
		return nil
	}()
	if seg != nil {
		p.mu.Unlock()
		return seg, nil
	}

	res := make(chan *SegmentFinalized)
	req2 := nextSegmentRequest{
		prevID: prevID,
		res:    res,
	}
	p.nextSegmentsOnHold[req2] = struct{}{}
	p.mu.Unlock() // Must be unlocked here.

	res3, ok := <-res
	if !ok {
		return nil, context.Canceled
	}
	return res3, nil
}
