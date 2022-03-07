package gortsplib

import (
	"fmt"
	"nvr/pkg/video/gortsplib/pkg/sdp"
	"strconv"

	psdp "github.com/pion/sdp/v3"
)

// Tracks is a list of tracks.
type Tracks []Track

// ReadTracks decodes tracks from the SDP format.
func ReadTracks(byts []byte) (Tracks, error) {
	var sd sdp.SessionDescription
	err := sd.Unmarshal(byts)
	if err != nil {
		return nil, err
	}

	tracks := make(Tracks, len(sd.MediaDescriptions))

	for i, md := range sd.MediaDescriptions {
		t, err := newTrackFromMediaDescription(md)
		if err != nil {
			return nil, fmt.Errorf("unable to parse track %d: %w", i, err)
		}

		tracks[i] = t
	}

	return tracks, nil
}

func (ts Tracks) clone() Tracks {
	ret := make(Tracks, len(ts))
	for i, track := range ts {
		ret[i] = track.clone()
	}
	return ret
}

func (ts Tracks) setControls() {
	for i, t := range ts {
		t.setControl("trackID=" + strconv.FormatInt(int64(i), 10))
	}
}

// Write encodes tracks in the SDP format.
func (ts Tracks) Write() []byte {
	address := "0.0.0.0"

	sout := &sdp.SessionDescription{
		SessionName: psdp.SessionName("Stream"),
		Origin: psdp.Origin{
			Username:       "-",
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: "127.0.0.1",
		},
		// required by Darwin Streaming Server
		ConnectionInformation: &psdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &psdp.Address{Address: address},
		},
		TimeDescriptions: []psdp.TimeDescription{
			{Timing: psdp.Timing{0, 0}}, //nolint:govet
		},
	}

	for _, track := range ts {
		sout.MediaDescriptions = append(sout.MediaDescriptions, track.mediaDescription())
	}

	byts, _ := sout.Marshal()
	return byts
}