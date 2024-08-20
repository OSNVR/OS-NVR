package hls

import (
	"errors"
	"io"
	"nvr/pkg/video/gortsplib"
	"strconv"
	"time"
)

type partsReader struct {
	parts   []*MuxerPartFinalized
	curPart int
	curPos  int
}

func (mbr *partsReader) Read(p []byte) (int, error) {
	n := 0
	lenp := len(p)

	for {
		if mbr.curPart >= len(mbr.parts) {
			return n, io.EOF
		}

		copied := copy(p[n:], mbr.parts[mbr.curPart].renderedContent[mbr.curPos:])
		mbr.curPos += copied
		n += copied

		if mbr.curPos == len(mbr.parts[mbr.curPart].renderedContent) {
			mbr.curPart++
			mbr.curPos = 0
		}

		if n == lenp {
			return n, nil
		}
	}
}

type Segment struct {
	ID             uint64
	muxerID        uint16
	StartTime      time.Time // Segment start time.
	startDTS       time.Duration
	muxerStartTime int64
	segmentMaxSize uint64
	audioTrack     *gortsplib.TrackMPEG4Audio
	genPartID      func() uint64
	playlist       *playlist

	name        string
	size        uint64
	Parts       []*MuxerPartFinalized
	currentPart *MuxerPart
}

func newSegment(
	id uint64,
	muxerID uint16,
	startTime time.Time,
	startDTS time.Duration,
	muxerStartTime int64,
	segmentMaxSize uint64,
	audioTrack *gortsplib.TrackMPEG4Audio,
	genPartID func() uint64,
	playlist *playlist,
) *Segment {
	s := &Segment{
		ID:             id,
		muxerID:        muxerID,
		StartTime:      startTime,
		startDTS:       startDTS,
		muxerStartTime: muxerStartTime,
		segmentMaxSize: segmentMaxSize,
		audioTrack:     audioTrack,
		genPartID:      genPartID,
		playlist:       playlist,
		name:           "seg" + strconv.FormatUint(id, 10),
	}

	s.currentPart = newPart(
		audioTrack,
		s.muxerStartTime,
		s.genPartID(),
	)

	return s
}

func (s Segment) finalize(nextVideoSample *VideoSample) (*SegmentFinalized, error) {
	partFinalized, err := s.currentPart.finalize()
	if err != nil {
		return nil, err
	}
	if partFinalized.renderedContent != nil {
		s.playlist.partFinalized(partFinalized)
		s.Parts = append(s.Parts, partFinalized)
	}
	renderedDuration := time.Duration(nextVideoSample.DTS-s.muxerStartTime) - s.startDTS

	return &SegmentFinalized{
		ID:               s.ID,
		muxerID:          s.muxerID,
		StartTime:        s.StartTime,
		name:             s.name,
		Parts:            s.Parts,
		RenderedDuration: renderedDuration,
	}, nil
}

// ErrMaximumSegmentSize reached maximum segment size.
var ErrMaximumSegmentSize = errors.New("reached maximum segment size")

func (s *Segment) writeH264(sample *VideoSample, adjustedPartDuration time.Duration) error {
	size := uint64(len(sample.AVCC))

	if (s.size + size) > s.segmentMaxSize {
		return ErrMaximumSegmentSize
	}

	s.currentPart.writeH264(sample)

	s.size += size

	// switch part
	if s.currentPart.duration() >= adjustedPartDuration {
		partFinalized, err := s.currentPart.finalize()
		if err != nil {
			return err
		}

		s.Parts = append(s.Parts, partFinalized)
		s.playlist.partFinalized(partFinalized)

		s.currentPart = newPart(
			s.audioTrack,
			s.muxerStartTime,
			s.genPartID(),
		)
	}

	return nil
}

func (s *Segment) writeAAC(sample *AudioSample) error {
	size := uint64(len(sample.AU))
	if (s.size + size) > s.segmentMaxSize {
		return ErrMaximumSegmentSize
	}
	s.size += size

	s.currentPart.writeAAC(sample)

	return nil
}

type SegmentFinalized struct {
	ID        uint64
	muxerID   uint16
	StartTime time.Time
	name      string
	Parts     []*MuxerPartFinalized

	RenderedDuration time.Duration
}

func (s *SegmentFinalized) reader() io.Reader {
	return &partsReader{parts: s.Parts}
}

func (s *SegmentFinalized) getRenderedDuration() time.Duration {
	return s.RenderedDuration
}
