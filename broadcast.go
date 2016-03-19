package main

import (
	"./ebml"
	"errors"
)

type viewer struct {
	// If `force` is `false`, this function may return `false` to signal that it
	// cannot write any more data. The stream will resynchronize at next keyframe.
	write func(data []byte, force bool) bool
	// Viewers may hop between streams, but should only receive headers once.
	// This includes track info, as codecs must stay the same between segments.
	skipHeaders bool
	// We group blocks into indeterminate-length clusters. So long as
	// the cluster's timecode has not changed, there's no need to start a new one.
	skipCluster bool
	// To avoid decoding errors due to missing reference frames, the first
	// frame of each track received by a viewer must be a keyframe.
	// Each track for which a keyframe has been sent is marked by a bit here.
	seenKeyframes uint32
}

type Broadcast struct {
	viewers map[chan<- []byte]*viewer

	// Set to `true` if there will be no more data on this stream.
	// All viewers will receive an empty bytearray and must disconnect.
	Done bool

	Width  uint // Of the last video track.
	Height uint // It is assumed there is only one, as having more is pointless.

	buffer []byte
	header []byte // The EBML (DocType) tag.
	tracks []byte // The beginning of the Segment (Tracks + Info).

	time struct {
		last  uint64 // Last seen block timecode.
		shift uint64 // Difference between sent and received timecodes.
		recv  uint64 // Last received cluster timecode.
		sent  uint64 // Last sent cluster timecode. (All viewers receive same timecodes.)
	}
}

func NewBroadcast() *Broadcast {
	cast := Broadcast{}
	cast.viewers = make(map[chan<- []byte]*viewer)
	return &cast
}

func (cast *Broadcast) Close() {
	cast.Done = true

	for _, cb := range cast.viewers {
		cb.write([]byte{}, false)
	}
}

func (cast *Broadcast) Connect(ch chan<- []byte, skipHeaders bool) {
	write := func(data []byte, force bool) bool {
		// `Broadcast.Write` emits data in block-sized chunks.
		// Thus the buffer size is measured in frames, not bytes.
		if !force && len(ch) == cap(ch) {
			return false
		}

		ch <- data
		return true
	}

	cast.viewers[ch] = &viewer{write, skipHeaders, false, 0}
	if !skipHeaders && len(cast.header) != 0 {
		write(cast.header, true)
	}
}

func (cast *Broadcast) Disconnect(ch chan<- []byte) {
	delete(cast.viewers, ch)
}

func (cast *Broadcast) Write(data []byte) (int, error) {
	cast.buffer = append(cast.buffer, data...)

	for {
		buf := cast.buffer
		tag := ebml.ParseTagIncomplete(buf)
		if tag.Consumed == 0 {
			return len(data), nil
		}

		if tag.ID == ebml.SegmentTag || tag.ID == ebml.TracksTag || tag.ID == ebml.ClusterTag {
			// Parse the contents of these tags in the same loop.
			buf = buf[:tag.Consumed]
			// Chrome crashes if an indeterminate length is not encoded as 0xFF.
			// If we want to recode it, we'll also need some space for a Void tag.
			if tag.Length == ebml.Indeterminate && tag.Consumed >= 7 {
				cast.buffer[4] = 0xFF
				cast.buffer[5] = ebml.VoidTag
				cast.buffer[6] = 0x80 | byte(tag.Consumed-7)
			}
		} else {
			total := tag.Length + uint64(tag.Consumed)
			if total > 1024*1024 {
				return 0, errors.New("data block too big")
			}

			if total > uint64(len(buf)) {
				return len(data), nil
			}

			buf = buf[:total]
		}

		switch tag.ID {
		case ebml.SeekHeadTag:
			// Disallow seeking.
		case ebml.ChaptersTag:
			// Disallow seeking again.
		case ebml.CuesTag:
			// Disallow even more seeking.
		case ebml.VoidTag:
			// Waste of space.
		case ebml.TagsTag:
			// Maybe later.
		case ebml.ClusterTag:
			// Ignore boundaries, we'll regroup the data anyway.
		case ebml.PrevSizeTag:
			// Disallow backward seeking too.

		case ebml.EBMLTag:
			// The header is the same in all WebM-s.
			if len(cast.header) == 0 {
				cast.header = append([]byte{}, buf...)
				for _, cb := range cast.viewers {
					if !cb.skipHeaders {
						cb.write(cast.header, true)
					}
				}
			}

		case ebml.SegmentTag:
			cast.tracks = append([]byte{}, buf...)
			// Will recalculate this when the first block arrives.
			cast.time.shift = 0

		case ebml.InfoTag:
			// Default timecode resolution in Matroska is 1 ms. This value is required
			// in WebM; we'll check just in case. Obviously, our timecode rewriting
			// logic won't work with non-millisecond resolutions.
			var scale uint64 = 0

			for buf2 := tag.Contents(buf); len(buf2) != 0; {
				tag2 := ebml.ParseTag(buf2)

				switch tag2.ID {
				case 0:
					return 0, errors.New("malformed EBML")

				case ebml.DurationTag:
					total := tag2.Length + uint64(tag2.Consumed) - 2
					if total > 0x7F {
						// I'd rather avoid shifting memory. What kind of integer
						// needs 128 bytes, anyway?
						return 0, errors.New("EBML Duration too large")
					}
					// Live streams must not have a duration.
					buf2[0] = ebml.VoidTag
					buf2[1] = 0x80 | byte(total)

				case ebml.TimecodeScaleTag:
					scale = ebml.ParseFixedUint(tag2.Contents(buf2))
				}

				buf2 = tag2.Skip(buf2)
			}

			if scale != 1000000 {
				return 0, errors.New("invalid timecode scale")
			}

			cast.tracks = append(cast.tracks, buf...)

		case ebml.TrackEntryTag:
			// Since `viewer.seenKeyframes` is a 32-bit vector,
			// we need to check that there are at most 32 tracks.
			for buf2 := tag.Contents(buf); len(buf2) != 0; {
				tag2 := ebml.ParseTag(buf2)

				switch tag2.ID {
				case 0:
					return 0, errors.New("malformed EBML")

				case ebml.TrackNumberTag:
					// go needs sizeof.
					if t := ebml.ParseFixedUint(tag2.Contents(buf2)); t >= 32 {
						return 0, errors.New("too many tracks?")
					}

				case ebml.VideoTag:
					// While we're here, let's grab some metadata, too.
					for buf3 := tag2.Contents(buf2); len(buf3) != 0; {
						tag3 := ebml.ParseTag(buf3)

						switch tag3.ID {
						case 0:
							return 0, errors.New("malformed EBML")

						case ebml.PixelWidthTag:
							cast.Width = uint(ebml.ParseFixedUint(tag3.Contents(buf3)))

						case ebml.PixelHeightTag:
							cast.Height = uint(ebml.ParseFixedUint(tag3.Contents(buf3)))
						}

						buf3 = tag3.Skip(buf3)
					}
				}

				buf2 = tag2.Skip(buf2)
			}

			cast.tracks = append(cast.tracks, buf...)

		case ebml.TracksTag:
			cast.tracks = append(cast.tracks, buf...)

		case ebml.TimecodeTag:
			// Will reencode it when sending a Cluster.
			cast.time.recv = ebml.ParseFixedUint(tag.Contents(buf))

		case ebml.BlockGroupTag, ebml.SimpleBlockTag:
			key := false
			block := tag.Contents(buf)

			if tag.ID == ebml.BlockGroupTag {
				key, block = true, nil

				for buf2 := tag.Contents(buf); len(buf2) != 0; {
					tag2 := ebml.ParseTag(buf2)

					switch tag2.ID {
					case 0:
						return 0, errors.New("malformed EBML")

					case ebml.BlockTag:
						block = tag2.Contents(buf2)

					case ebml.ReferenceBlockTag:
						// Keyframes, by definition, have no reference frame.
						key = ebml.ParseFixedUint(tag2.Contents(buf2)) == 0
					}

					buf2 = tag2.Skip(buf2)
				}

				if block == nil {
					return 0, errors.New("a BlockGroup contains no Blocks")
				}
			}

			track := ebml.ParseUint(block)
			if track.Consumed == 0 || track.Value >= 32 || len(block) < track.Consumed+3 {
				return 0, errors.New("invalid track")
			}

			// Always 0 in a Block, 1 in a keyframe SimpleBlock.
			key = key || block[track.Consumed+2]&0x80 != 0

			timecode := uint64(block[track.Consumed+0])<<8 | uint64(block[track.Consumed+1])

			// Adding the shift here instead of accounting for it in `cast.time.recv`
			// allows the broadcaster to insert discontinuities between clusters.
			if cast.time.recv+cast.time.shift+timecode < cast.time.last {
				cast.time.shift = cast.time.last - cast.time.recv - timecode
			}

			cast.time.last = cast.time.recv + cast.time.shift + timecode

			// Keep the block's timecode offset the same, but shift the cluster's timecode.
			timecode = cast.time.recv + cast.time.shift
			cluster := []byte{
				ebml.ClusterTag >> 24 & 0xFF,
				ebml.ClusterTag >> 16 & 0xFF,
				ebml.ClusterTag >> 8 & 0xFF,
				ebml.ClusterTag & 0xFF, 0xFF,
				ebml.TimecodeTag, 0x88,
				byte(timecode >> 56), byte(timecode >> 48),
				byte(timecode >> 40), byte(timecode >> 32),
				byte(timecode >> 24), byte(timecode >> 16),
				byte(timecode >> 8), byte(timecode),
			}

			trackMask := uint32(1) << track.Value
			for _, cb := range cast.viewers {
				if key {
					cb.seenKeyframes |= trackMask
				}

				if cb.seenKeyframes&trackMask != 0 {
					if !cb.skipHeaders {
						if !cb.write(cast.tracks, true) {
							continue
						}

						cb.skipHeaders = true
						cb.skipCluster = false
					}

					if !cb.skipCluster || timecode != cast.time.sent {
						cb.skipCluster = cb.write(cluster, false)
					}

					if !cb.skipCluster || !cb.write(buf, false) {
						cb.seenKeyframes &= ^trackMask
					}
				}
			}

			cast.time.sent = timecode

		default:
			return 0, errors.New("unknown EBML tag")
		}

		cast.buffer = cast.buffer[len(buf):]
	}
}