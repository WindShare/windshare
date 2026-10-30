package main

import (
	"encoding/binary"
	"fmt"
)

const (
	packetHeader        = 12
	chunkHeader         = 4
	dataType            = 0
	sackType            = 3
	interleavedDataType = 64
)

type dataChunk struct {
	TSN  uint32
	Kind byte
}
type sackChunk struct {
	Cumulative uint32
	Gaps       [][2]uint16
	Duplicates []uint32
}
type packetView struct {
	Data  []dataChunk
	SACKs []sackChunk
}

func parsePacket(p []byte) (packetView, error) {
	var v packetView
	if len(p) < packetHeader {
		return v, fmt.Errorf("short SCTP packet: %d", len(p))
	}
	for at := packetHeader; at < len(p); {
		if at+chunkHeader > len(p) {
			return v, fmt.Errorf("truncated chunk at %d", at)
		}
		size := int(binary.BigEndian.Uint16(p[at+2 : at+4]))
		if size < chunkHeader || at+size > len(p) {
			return v, fmt.Errorf("invalid chunk length %d at %d", size, at)
		}
		c := p[at : at+size]
		switch c[0] {
		case dataType, interleavedDataType:
			if size < 16 {
				return v, fmt.Errorf("short DATA chunk")
			}
			v.Data = append(v.Data, dataChunk{binary.BigEndian.Uint32(c[4:8]), c[0]})
		case sackType:
			if size < 16 {
				return v, fmt.Errorf("short SACK chunk")
			}
			gaps, dups := int(binary.BigEndian.Uint16(c[12:14])), int(binary.BigEndian.Uint16(c[14:16]))
			if 16+4*(gaps+dups) > size {
				return v, fmt.Errorf("truncated SACK blocks")
			}
			s := sackChunk{Cumulative: binary.BigEndian.Uint32(c[4:8])}
			for i := range gaps {
				pos := 16 + 4*i
				s.Gaps = append(s.Gaps, [2]uint16{binary.BigEndian.Uint16(c[pos : pos+2]), binary.BigEndian.Uint16(c[pos+2 : pos+4])})
			}
			for i := range dups {
				pos := 16 + 4*(gaps+i)
				s.Duplicates = append(s.Duplicates, binary.BigEndian.Uint32(c[pos:pos+4]))
			}
			v.SACKs = append(v.SACKs, s)
		}
		at += (size + 3) &^ 3
	}
	return v, nil
}
func (s sackChunk) acknowledges(tsn uint32) bool {
	if int32(tsn-s.Cumulative) <= 0 {
		return true
	}
	offset := tsn - s.Cumulative
	for _, gap := range s.Gaps {
		if offset >= uint32(gap[0]) && offset <= uint32(gap[1]) {
			return true
		}
	}
	return false
}
