package data

import (
	"time"

	"github.com/pion/rtp"
)

// Data is the Data unit routed across the server.
// it must contain one or more of the following:
// - a single RTP packet
// - a group of H264 NALUs (grouped by timestamp)
// - a single AAC AU.
type Data interface {
	GetTrackID() int
	GetRTPPackets() []*rtp.Packet
	GetNTP() time.Time
}

type H264 struct {
	TrackID    int
	RTPPackets []*rtp.Packet
	Ntp        time.Time
	Pts        time.Duration
	Nalus      [][]byte
}

func (d *H264) GetTrackID() int {
	return d.TrackID
}

func (d *H264) GetRTPPackets() []*rtp.Packet {
	return d.RTPPackets
}

func (d *H264) GetNTP() time.Time {
	return d.Ntp
}

type MPEG4Audio struct {
	TrackID    int
	RTPPackets []*rtp.Packet
	Ntp        time.Time
	Pts        time.Duration
	Aus        [][]byte
}

func (d *MPEG4Audio) GetTrackID() int {
	return d.TrackID
}

func (d *MPEG4Audio) GetRTPPackets() []*rtp.Packet {
	return d.RTPPackets
}

func (d *MPEG4Audio) GetNTP() time.Time {
	return d.Ntp
}
