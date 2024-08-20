package hls

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"nvr/pkg/log"
	"nvr/pkg/video/data"
	"nvr/pkg/video/gortsplib"
	"nvr/pkg/video/gortsplib/pkg/mpeg4audio"
	"sync"
	"time"
)

// MuxerFileResponse is a response of the Muxer's File() func.
type MuxerFileResponse struct {
	Status int
	Header map[string]string
	Body   io.Reader
}

func MuxerFileReponseCancelled() MuxerFileResponse {
	return MuxerFileResponse{Status: http.StatusInternalServerError}
}

// Muxer is a HLS muxer.
type Muxer struct {
	playlist  *playlist
	segmenter *segmenter
	logf      log.Func

	videoTrack          *gortsplib.TrackH264
	videoTrackID        int
	audioTrack          *gortsplib.TrackMPEG4Audio
	audioTrackID        int
	videoStartPTSFilled bool
	videoStartPTS       time.Duration
	audioStartPTSFilled bool
	audioStartPTS       time.Duration

	mutex        sync.Mutex
	videoLastSPS []byte
	videoLastPPS []byte
	initContent  []byte
}

// ErrTrackInvalid invalid H264 track: SPS or PPS not provided into the SDP.
var ErrTrackInvalid = errors.New("invalid H264 track: SPS or PPS not provided into the SDP")

// NewMuxer allocates a Muxer.
func NewMuxer(
	id uint16,
	segmentCount int,
	segmentDuration time.Duration,
	partDuration time.Duration,
	segmentMaxSize uint64,

	pathLogf log.Func,
	tracks gortsplib.Tracks,
) (*Muxer, error) {
	videoTrack, videoTrackID, audioTrack, audioTrackID, err := parseTracks(tracks)
	if err != nil {
		return nil, fmt.Errorf("parse tracks: %w", err)
	}

	logf := func(level log.Level, format string, a ...interface{}) {
		pathLogf(level, "HLS: "+format, a...)
	}

	playlist := newPlaylist(id, segmentCount)
	segmenter := newSegmenter(
		id,
		time.Now().UnixNano(),
		segmentDuration,
		partDuration,
		segmentMaxSize,
		videoTrack,
		audioTrack,
		playlist,
	)

	return &Muxer{
		playlist:     playlist,
		segmenter:    segmenter,
		logf:         logf,
		videoTrack:   videoTrack,
		videoTrackID: videoTrackID,
		audioTrack:   audioTrack,
		audioTrackID: audioTrackID,
	}, nil
}

// OnSegmentFinalizedFunc is injected by core.
type OnSegmentFinalizedFunc func([]SegmentOrGap)

func (m *Muxer) Cancel() {
	m.playlist.cancel()
}

// WriteData is called by stream.
func (m *Muxer) WriteData(d data.Data) error {
	if m.videoTrack != nil && d.GetTrackID() == m.videoTrackID {
		tdata := d.(*data.H264) //nolint:forcetypeassert

		if tdata.Nalus == nil {
			return nil
		}

		if !m.videoStartPTSFilled {
			m.videoStartPTSFilled = true
			m.videoStartPTS = tdata.Pts
		}
		pts := tdata.Pts - m.videoStartPTS

		err := m.segmenter.writeH264(tdata.Ntp, pts, tdata.Nalus)
		if err != nil {
			return fmt.Errorf("muxer error: %w", err)
		}
	} else if m.audioTrack != nil && d.GetTrackID() == m.audioTrackID {
		tdata := d.(*data.MPEG4Audio) //nolint:forcetypeassert

		if tdata.Aus == nil {
			return nil
		}

		if !m.audioStartPTSFilled {
			m.audioStartPTSFilled = true
			m.audioStartPTS = tdata.Pts
		}
		pts := tdata.Pts - m.audioStartPTS

		for i, au := range tdata.Aus {
			err := m.segmenter.writeAAC(
				pts+time.Duration(i)*mpeg4audio.SamplesPerAccessUnit*
					time.Second/time.Duration(m.audioTrack.ClockRate()),
				au)
			if err != nil {
				return fmt.Errorf("muxer error: %w", err)
			}
		}
	}
	return nil
}

func (m *Muxer) OnRequest(file string, p url.Values) MuxerFileResponse {
	msn := func() string {
		if len(p["_HLS_msn"]) > 0 {
			return p["_HLS_msn"][0]
		}
		return ""
	}()
	part := func() string {
		if len(p["_HLS_part"]) > 0 {
			return p["_HLS_part"][0]
		}
		return ""
	}()
	skip := func() string {
		if len(p["_HLS_skip"]) > 0 {
			return p["_HLS_skip"][0]
		}
		return ""
	}()

	return m.File(file, msn, part, skip)
}

func (m *Muxer) File(
	name string,
	msn string,
	part string,
	skip string,
) MuxerFileResponse {
	if name == "index.m3u8" {
		return primaryPlaylist(m.videoTrack, m.audioTrack)
	}

	if name == "init.mp4" {
		m.mutex.Lock()
		defer m.mutex.Unlock()

		sps := m.videoTrack.SPS

		if m.initContent == nil ||
			(!bytes.Equal(m.videoLastSPS, sps) ||
				!bytes.Equal(m.videoLastPPS, m.videoTrack.PPS)) {
			initContent, err := generateInit(m.videoTrack, m.audioTrack)
			if err != nil {
				m.logf(log.LevelError, "generate init.mp4: %w", err)
				return MuxerFileResponse{Status: http.StatusInternalServerError}
			}
			m.videoLastSPS = m.videoTrack.SPS
			m.videoLastPPS = m.videoTrack.PPS
			m.initContent = initContent
		}

		return MuxerFileResponse{
			Status: http.StatusOK,
			Header: map[string]string{
				"Content-Type": "video/mp4",
			},
			Body: bytes.NewReader(m.initContent),
		}
	}

	return m.playlist.file(name, msn, part, skip)
}

// VideoTrack returns the stream video track.
func (m *Muxer) VideoTrack() *gortsplib.TrackH264 {
	return m.videoTrack
}

// AudioTrack returns the stream audio track.
func (m *Muxer) AudioTrack() *gortsplib.TrackMPEG4Audio {
	return m.audioTrack
}

// WaitForSegFinalized blocks until a new segment has been finalized.
func (m *Muxer) WaitForSegFinalized() {
	m.playlist.waitForSegFinalized()
}

// NextSegment returns the first segment with a ID greater than prevID.
// Will wait for new segments if the next segment isn't cached.
func (m *Muxer) NextSegment(maybePrevSeg *SegmentFinalized) (*SegmentFinalized, error) {
	return m.playlist.nextSegment(maybePrevSeg)
}

// VideoTimescale the number of time units that pass per second.
const VideoTimescale = 90000

// Sample .
type Sample interface {
	private()
}

// VideoSample Timestamps are in UnixNano.
type VideoSample struct {
	PTS        int64
	DTS        int64
	AVCC       []byte
	IdrPresent bool

	Duration time.Duration
}

func (VideoSample) private() {}

// AudioSample Timestamps are in UnixNano.
type AudioSample struct {
	AU  []byte
	PTS int64

	NextPTS int64
}

// Duration sample duration.
func (s AudioSample) Duration() time.Duration {
	return time.Duration(s.NextPTS - s.PTS)
}

func (AudioSample) private() {}

// Errors.
var (
	ErrTooManyTracks = errors.New("too many tracks")
	ErrNoTracks      = errors.New("the stream doesn't contain an H264 track or an AAC track")
)

func parseTracks(tracks gortsplib.Tracks) (
	*gortsplib.TrackH264, int,
	*gortsplib.TrackMPEG4Audio,
	int,
	error,
) {
	var videoTrack *gortsplib.TrackH264
	videoTrackID := -1
	var audioTrack *gortsplib.TrackMPEG4Audio
	audioTrackID := -1

	for i, track := range tracks {
		switch tt := track.(type) {
		case *gortsplib.TrackH264:
			if videoTrack != nil {
				return nil, 0, nil, 0,
					fmt.Errorf("can't encode track %d with HLS: %w", i+1, ErrTooManyTracks)
			}

			videoTrack = tt
			videoTrackID = i

		case *gortsplib.TrackMPEG4Audio:
			if audioTrack != nil {
				return nil, 0, nil, 0,
					fmt.Errorf("can't encode track %d with HLS: %w", i+1, ErrTooManyTracks)
			}

			audioTrack = tt
			audioTrackID = i
		}
	}

	if videoTrack == nil && audioTrack == nil {
		return nil, 0, nil, 0, ErrNoTracks
	}

	return videoTrack, videoTrackID, audioTrack, audioTrackID, nil
}
