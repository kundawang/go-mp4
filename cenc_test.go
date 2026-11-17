package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cencFixtureConfig parameterizes buildCencFixture so that error cases can
// be produced by mutating a valid default configuration.
type cencFixtureConfig struct {
	videoTenc   *Tenc // nil omits the tenc box of track 1
	audioTenc   *Tenc
	audioScheme [4]byte
	moovPsshB   *Pssh // third pssh box of moov (system ID B)
	moofPssh    *Pssh // pssh box placed into the second moof
	saioDelta   uint64
}

// cencFixture is a fixed two-track, two-fragment CENC MP4 file built in
// memory, together with the absolute offsets of its relevant boxes.
type cencFixture struct {
	data           []byte
	moovOffset     uint64
	psshMoovOffset []uint64 // offsets of the pssh boxes inside moov
	moofOffset     [2]uint64
	moofPsshOffset uint64 // offset of the pssh box inside the second moof
	auxOffset      [2][2]uint64
	sencTrafOffset [2]uint64 // senc box offsets, one per moof (track 1 only)
}

var (
	cencVideoKID = [16]byte{0x01, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	cencAudioKID = [16]byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	// Widevine and PlayReady system IDs
	cencSystemIDA    = [16]byte{0xed, 0xef, 0x8b, 0xa9, 0x79, 0xd6, 0x4a, 0xce, 0xa3, 0xc8, 0x27, 0xdc, 0xd5, 0x1d, 0x21, 0xed}
	cencSystemIDB    = [16]byte{0x9a, 0x04, 0xf0, 0x79, 0x98, 0x40, 0x42, 0x86, 0xab, 0x92, 0xe6, 0x5b, 0xe0, 0x88, 0x5f, 0x95}
	cencAudioConstIV = []byte{0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf}
)

func defaultCencFixtureConfig() *cencFixtureConfig {
	return &cencFixtureConfig{
		videoTenc: &Tenc{
			FullBox:                FullBox{Version: 0},
			DefaultIsProtected:     1,
			DefaultPerSampleIVSize: 8,
			DefaultKID:             cencVideoKID,
		},
		audioTenc: &Tenc{
			FullBox:                FullBox{Version: 1},
			DefaultCryptByteBlock:  1,
			DefaultIsProtected:     1,
			DefaultPerSampleIVSize: 0,
			DefaultKID:             cencAudioKID,
			DefaultConstantIVSize:  16,
			DefaultConstantIV:      cencAudioConstIV,
		},
		audioScheme: [4]byte{'c', 'b', 'c', 's'},
		moovPsshB: &Pssh{
			FullBox:  FullBox{Version: 1},
			SystemID: cencSystemIDB,
			KIDCount: 2,
			KIDs:     []PsshKID{{KID: cencVideoKID}, {KID: cencAudioKID}},
			DataSize: 3,
			Data:     []byte{0x01, 0x02, 0x03},
		},
		moofPssh: &Pssh{ // identical duplicate of moovPsshB
			FullBox:  FullBox{Version: 1},
			SystemID: cencSystemIDB,
			KIDCount: 2,
			KIDs:     []PsshKID{{KID: cencVideoKID}, {KID: cencAudioKID}},
			DataSize: 3,
			Data:     []byte{0x01, 0x02, 0x03},
		},
	}
}

func cencPayload(t *testing.T, box IImmutableBox) []byte {
	t.Helper()
	buf := bytes.NewBuffer(nil)
	_, err := Marshal(buf, box, Context{})
	require.NoError(t, err)
	return buf.Bytes()
}

func cencBox(boxType string, payload []byte) []byte {
	buf := bytes.NewBuffer(nil)
	_ = binary.Write(buf, binary.BigEndian, uint32(8+len(payload)))
	buf.WriteString(boxType)
	buf.Write(payload)
	return buf.Bytes()
}

func cencConcat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func buildCencTrak(t *testing.T, trackID uint32, timescale uint32, entry []byte) []byte {
	tkhd := cencBox("tkhd", cencPayload(t, &Tkhd{
		TrackID: trackID,
		Matrix:  [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}))
	mdhd := cencBox("mdhd", cencPayload(t, &Mdhd{Timescale: timescale}))
	stsd := cencBox("stsd", cencConcat(
		cencPayload(t, &Stsd{EntryCount: 1}),
		entry,
	))
	stts := cencBox("stts", cencPayload(t, &Stts{
		EntryCount: 1,
		Entries:    []SttsEntry{{SampleCount: 4, SampleDelta: 1024}},
	}))
	stsc := cencBox("stsc", cencPayload(t, &Stsc{
		EntryCount: 1,
		Entries:    []StscEntry{{FirstChunk: 1, SamplesPerChunk: 4, SampleDescriptionIndex: 1}},
	}))
	stsz := cencBox("stsz", cencPayload(t, &Stsz{SampleSize: 100, SampleCount: 4}))
	stco := cencBox("stco", cencPayload(t, &Stco{EntryCount: 1, ChunkOffset: []uint32{0}}))
	stbl := cencBox("stbl", cencConcat(stsd, stts, stsc, stsz, stco))
	minf := cencBox("minf", stbl)
	mdia := cencBox("mdia", cencConcat(mdhd, minf))
	return cencBox("trak", cencConcat(tkhd, mdia))
}

func buildCencVideoEntry(t *testing.T, tenc *Tenc) []byte {
	frma := cencBox("frma", cencPayload(t, &Frma{DataFormat: [4]byte{'a', 'v', 'c', '1'}}))
	schm := cencBox("schm", cencPayload(t, &Schm{
		SchemeType:    [4]byte{'c', 'e', 'n', 'c'},
		SchemeVersion: 0x00010000,
	}))
	var schiPayload []byte
	if tenc != nil {
		schiPayload = cencBox("tenc", cencPayload(t, tenc))
	}
	schi := cencBox("schi", schiPayload)
	sinf := cencBox("sinf", cencConcat(frma, schm, schi))
	return cencBox("encv", cencConcat(
		cencPayload(t, &VisualSampleEntry{
			SampleEntry: SampleEntry{
				AnyTypeBox:         AnyTypeBox{Type: BoxTypeEncv()},
				DataReferenceIndex: 1,
			},
			Width:           320,
			Height:          240,
			Horizresolution: 0x00480000,
			Vertresolution:  0x00480000,
			FrameCount:      1,
			Depth:           24,
			PreDefined3:     -1,
		}),
		sinf,
	))
}

func buildCencAudioEntry(t *testing.T, tenc *Tenc, scheme [4]byte) []byte {
	frma := cencBox("frma", cencPayload(t, &Frma{DataFormat: [4]byte{'m', 'p', '4', 'a'}}))
	schm := cencBox("schm", cencPayload(t, &Schm{
		SchemeType:    scheme,
		SchemeVersion: 0x00010000,
	}))
	var schiPayload []byte
	if tenc != nil {
		schiPayload = cencBox("tenc", cencPayload(t, tenc))
	}
	schi := cencBox("schi", schiPayload)
	sinf := cencBox("sinf", cencConcat(frma, schm, schi))
	return cencBox("enca", cencConcat(
		cencPayload(t, &AudioSampleEntry{
			SampleEntry: SampleEntry{
				AnyTypeBox:         AnyTypeBox{Type: BoxTypeEnca()},
				DataReferenceIndex: 1,
			},
			ChannelCount: 2,
			SampleSize:   16,
			SampleRate:   48000 << 16,
		}),
		sinf,
	))
}

// buildCencMoof builds one moof box with two trafs (track 1 and track 2).
// saioOffsets holds the raw saio values for each traf.
func buildCencMoof(t *testing.T, seq uint32, pssh *Pssh, saioOffsets [2]uint32) (moof []byte, psshOffsetInMoof uint64, sencOffsetInMoof uint64) {
	mfhd := cencBox("mfhd", cencPayload(t, &Mfhd{SequenceNumber: seq}))

	var psshBox []byte
	head := mfhd
	if pssh != nil {
		psshBox = cencBox("pssh", cencPayload(t, pssh))
		psshOffsetInMoof = 8 + uint64(len(head))
		head = cencConcat(head, psshBox)
	}

	tfhd1 := cencBox("tfhd", cencPayload(t, &Tfhd{
		FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, // default-base-is-moof
		TrackID: 1,
	}))
	tfdt1 := cencBox("tfdt", cencPayload(t, &Tfdt{BaseMediaDecodeTimeV0: 1024 * 4 * uint32(seq-1)}))
	saiz1 := cencBox("saiz", cencPayload(t, &Saiz{
		DefaultSampleInfoSize: 8,
		SampleCount:           2,
	}))
	saio1 := cencBox("saio", cencPayload(t, &Saio{
		EntryCount: 1,
		OffsetV0:   []uint32{saioOffsets[0]},
	}))
	senc1 := cencBox("senc", []byte{0xde, 0xad, 0xbe, 0xef})
	traf1Payload := cencConcat(tfhd1, tfdt1, saiz1, saio1, senc1)
	sencOffsetInMoof = 8 + uint64(len(head)) + 8 + uint64(len(tfhd1)+len(tfdt1)+len(saiz1)+len(saio1))
	traf1 := cencBox("traf", traf1Payload)

	tfhd2 := cencBox("tfhd", cencPayload(t, &Tfhd{
		FullBox: FullBox{Flags: [3]byte{0x02, 0x00, 0x00}}, // default-base-is-moof
		TrackID: 2,
	}))
	tfdt2 := cencBox("tfdt", cencPayload(t, &Tfdt{BaseMediaDecodeTimeV0: 2048 * 2 * uint32(seq-1)}))
	saiz2 := cencBox("saiz", cencPayload(t, &Saiz{
		SampleCount:    2,
		SampleInfoSize: []uint8{16, 16},
	}))
	saio2 := cencBox("saio", cencPayload(t, &Saio{
		EntryCount: 1,
		OffsetV0:   []uint32{saioOffsets[1]},
	}))
	traf2 := cencBox("traf", cencConcat(tfhd2, tfdt2, saiz2, saio2))

	return cencBox("moof", cencConcat(head, traf1, traf2)), psshOffsetInMoof, sencOffsetInMoof
}

// buildCencFixture builds the fixed two-track, two-fragment file:
//
//	ftyp | moov (pssh A, pssh A dup, pssh B; trak 1 encv/cenc; trak 2 enca/cbcs)
//	     | moof1 (traf1: saiz+saio+senc, traf2: saiz+saio) | mdat1 (aux info)
//	     | moof2 (pssh B dup, traf1, traf2)               | mdat2 (aux info)
func buildCencFixture(t *testing.T, mutate func(*cencFixtureConfig)) *cencFixture {
	t.Helper()
	cfg := defaultCencFixtureConfig()
	if mutate != nil {
		mutate(cfg)
	}

	ftyp := cencBox("ftyp", cencPayload(t, &Ftyp{
		MajorBrand:   [4]byte{'i', 's', 'o', 'm'},
		MinorVersion: 1,
		CompatibleBrands: []CompatibleBrandElem{
			{CompatibleBrand: [4]byte{'i', 's', 'o', 'm'}},
			{CompatibleBrand: [4]byte{'c', 'e', 'n', 'c'}},
		},
	}))

	// moov
	mvhd := cencBox("mvhd", cencPayload(t, &Mvhd{
		Timescale:  1000,
		DurationV0: 8000,
		Rate:       0x00010000,
		Volume:     0x0100,
		Matrix:     [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}))
	psshA := &Pssh{
		FullBox:  FullBox{Version: 0},
		SystemID: cencSystemIDA,
		DataSize: 4,
		Data:     []byte{0x10, 0x20, 0x30, 0x40},
	}
	psshMoov := [][]byte{
		cencBox("pssh", cencPayload(t, psshA)),
		cencBox("pssh", cencPayload(t, psshA)), // identical duplicate
		cencBox("pssh", cencPayload(t, cfg.moovPsshB)),
	}
	trak1 := buildCencTrak(t, 1, 90000, buildCencVideoEntry(t, cfg.videoTenc))
	trak2 := buildCencTrak(t, 2, 48000, buildCencAudioEntry(t, cfg.audioTenc, cfg.audioScheme))
	moov := cencBox("moov", cencConcat(append(append([][]byte{mvhd}, psshMoov...), trak1, trak2)...))

	fx := &cencFixture{moovOffset: uint64(len(ftyp))}
	off := fx.moovOffset + 8 + uint64(len(mvhd))
	for _, p := range psshMoov {
		fx.psshMoovOffset = append(fx.psshMoovOffset, off)
		off += uint64(len(p))
	}

	// moof1 + mdat1: aux info of traf1 (2 samples x 8 bytes) and traf2
	// (2 samples x 16 bytes) lives at the head of the mdat payload.
	moof1Offset := fx.moovOffset + uint64(len(moov))
	moof1, _, sencOff1 := buildCencMoof(t, 1, nil, [2]uint32{0, 0})
	aux11 := moof1Offset + uint64(len(moof1)) + 8
	aux12 := aux11 + 16
	moof1, _, sencOff1 = buildCencMoof(t, 1, nil, [2]uint32{
		uint32(aux11-moof1Offset) + uint32(cfg.saioDelta),
		uint32(aux12-moof1Offset) + uint32(cfg.saioDelta),
	})
	mdat1 := cencBox("mdat", cencConcat(
		bytes.Repeat([]byte{0x11}, 16),
		bytes.Repeat([]byte{0x22}, 32),
		bytes.Repeat([]byte{0x33}, 64), // media bytes; never read
	))

	// moof2 + mdat2
	moof2Offset := moof1Offset + uint64(len(moof1)) + uint64(len(mdat1))
	moof2, psshOff2, sencOff2 := buildCencMoof(t, 2, cfg.moofPssh, [2]uint32{0, 0})
	aux21 := moof2Offset + uint64(len(moof2)) + 8
	aux22 := aux21 + 16
	moof2, psshOff2, sencOff2 = buildCencMoof(t, 2, cfg.moofPssh, [2]uint32{
		uint32(aux21 - moof2Offset),
		uint32(aux22 - moof2Offset),
	})
	mdat2 := cencBox("mdat", cencConcat(
		bytes.Repeat([]byte{0x44}, 16),
		bytes.Repeat([]byte{0x55}, 32),
		bytes.Repeat([]byte{0x66}, 64),
	))

	fx.moofOffset = [2]uint64{moof1Offset, moof2Offset}
	fx.moofPsshOffset = moof2Offset + psshOff2
	fx.auxOffset = [2][2]uint64{{aux11, aux12}, {aux21, aux22}}
	fx.sencTrafOffset = [2]uint64{moof1Offset + sencOff1, moof2Offset + sencOff2}
	fx.data = cencConcat(ftyp, moov, moof1, mdat1, moof2, mdat2)
	return fx
}

func TestReadCencManifest(t *testing.T) {
	fx := buildCencFixture(t, nil)
	m, err := ReadCencManifest(bytes.NewReader(fx.data))
	require.NoError(t, err)

	// tracks
	require.Len(t, m.Tracks, 2)

	video := m.FindTrack(1)
	require.NotNil(t, video)
	assert.True(t, video.Encrypted)
	require.Len(t, video.Entries, 1)
	ve := video.Entries[0]
	assert.Equal(t, [4]byte{'e', 'n', 'c', 'v'}, ve.EntryType)
	assert.Equal(t, [4]byte{'a', 'v', 'c', '1'}, ve.OriginalFormat)
	assert.Equal(t, [4]byte{'c', 'e', 'n', 'c'}, ve.SchemeType)
	assert.Equal(t, uint32(0x00010000), ve.SchemeVersion)
	assert.True(t, ve.SchemeKnown)
	require.NotNil(t, ve.Tenc)
	assert.Equal(t, uint8(1), ve.Tenc.IsProtected)
	assert.Equal(t, uint8(8), ve.Tenc.PerSampleIVSize)
	assert.Equal(t, cencVideoKID, ve.Tenc.KID)
	assert.Nil(t, ve.Tenc.ConstantIV)

	audio := m.FindTrack(2)
	require.NotNil(t, audio)
	assert.True(t, audio.Encrypted)
	require.Len(t, audio.Entries, 1)
	ae := audio.Entries[0]
	assert.Equal(t, [4]byte{'e', 'n', 'c', 'a'}, ae.EntryType)
	assert.Equal(t, [4]byte{'m', 'p', '4', 'a'}, ae.OriginalFormat)
	assert.Equal(t, [4]byte{'c', 'b', 'c', 's'}, ae.SchemeType)
	assert.True(t, ae.SchemeKnown)
	require.NotNil(t, ae.Tenc)
	assert.Equal(t, uint8(0), ae.Tenc.PerSampleIVSize)
	assert.Equal(t, cencAudioKID, ae.Tenc.KID)
	assert.Equal(t, cencAudioConstIV, ae.Tenc.ConstantIV)
	assert.Equal(t, uint8(1), ae.Tenc.CryptByteBlock)

	assert.Nil(t, m.FindTrack(3))

	// pssh boxes: 3 in moov (one identical duplicate) + 1 in moof2
	require.Len(t, m.PsshBoxes, 4)
	for i := 0; i < 3; i++ {
		assert.Equal(t, CencPsshInMoov, m.PsshBoxes[i].Source.Location)
		assert.Equal(t, fx.psshMoovOffset[i], m.PsshBoxes[i].Source.BoxOffset)
	}
	assert.Equal(t, cencSystemIDA, m.PsshBoxes[0].SystemID)
	assert.Equal(t, cencSystemIDA, m.PsshBoxes[1].SystemID)
	assert.Equal(t, m.PsshBoxes[0].KIDs, m.PsshBoxes[1].KIDs)
	assert.NotEqual(t, m.PsshBoxes[0].Source.BoxOffset, m.PsshBoxes[1].Source.BoxOffset)
	assert.Equal(t, cencSystemIDB, m.PsshBoxes[2].SystemID)
	assert.Equal(t, cencSystemIDB, m.PsshBoxes[3].SystemID)
	assert.Equal(t, CencPsshInMoof, m.PsshBoxes[3].Source.Location)
	assert.Equal(t, fx.moofPsshOffset, m.PsshBoxes[3].Source.BoxOffset)
	assert.Equal(t, fx.moofOffset[1], m.PsshBoxes[3].Source.MoofOffset)
	assert.Equal(t, [][16]byte{cencVideoKID, cencAudioKID}, m.PsshBoxes[3].KIDs)

	// fragments: 2 moofs x 2 trafs
	require.Len(t, m.Fragments, 4)
	for i, f := range m.Fragments {
		mi, ti := i/2, i%2
		assert.Equal(t, fx.moofOffset[mi], f.MoofOffset)
		assert.Equal(t, uint32(ti+1), f.TrackID)
		require.Len(t, f.AuxInfoOffsets, 1)
		assert.Equal(t, fx.auxOffset[mi][ti], f.AuxInfoOffsets[0])
		assert.Equal(t, uint32(2), f.SampleCount)
	}
	// track 1 fragments: default sample info size, senc present
	for _, i := range []int{0, 2} {
		f := m.Fragments[i]
		assert.Equal(t, uint8(8), f.DefaultSampleInfoSize)
		assert.Nil(t, f.SampleInfoSizes)
		assert.Equal(t, uint8(8), f.GetSampleInfoSize(0))
		assert.Equal(t, uint8(8), f.GetSampleInfoSize(1))
		assert.Equal(t, int64(fx.sencTrafOffset[i/2]), f.SencOffset)
	}
	// track 2 fragments: per-sample sizes override the default, no senc
	for _, i := range []int{1, 3} {
		f := m.Fragments[i]
		assert.Equal(t, uint8(0), f.DefaultSampleInfoSize)
		assert.Equal(t, []uint8{16, 16}, f.SampleInfoSizes)
		assert.Equal(t, uint8(16), f.GetSampleInfoSize(0))
		assert.Equal(t, uint8(16), f.GetSampleInfoSize(1))
		assert.Equal(t, int64(-1), f.SencOffset)
	}
}

// TestReadCencManifestRoundTrip checks that the boxes of the fixture still
// unmarshal to the original values with the pre-existing box API.
func TestReadCencManifestRoundTrip(t *testing.T) {
	fx := buildCencFixture(t, nil)
	r := bytes.NewReader(fx.data)

	bis, err := ExtractBoxesWithPayload(r, nil, []BoxPath{
		{BoxTypeMoov(), BoxTypePssh()},
		{BoxTypeMoov(), BoxTypeTrak(), BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv(), BoxTypeSinf(), BoxTypeSchi(), BoxTypeTenc()},
	})
	require.NoError(t, err)
	require.Len(t, bis, 4)

	pssh, ok := bis[0].Payload.(*Pssh)
	require.True(t, ok)
	assert.Equal(t, cencSystemIDA, pssh.SystemID)
	assert.Equal(t, []byte{0x10, 0x20, 0x30, 0x40}, pssh.Data)

	tenc, ok := bis[3].Payload.(*Tenc)
	require.True(t, ok)
	assert.Equal(t, uint8(8), tenc.DefaultPerSampleIVSize)
	assert.Equal(t, cencVideoKID, tenc.DefaultKID)
}

func TestReadCencManifestMissingTenc(t *testing.T) {
	fx := buildCencFixture(t, func(cfg *cencFixtureConfig) {
		cfg.videoTenc = nil
	})
	m, err := ReadCencManifest(bytes.NewReader(fx.data))
	require.NoError(t, err)
	video := m.FindTrack(1)
	require.NotNil(t, video)
	require.Len(t, video.Entries, 1)
	assert.Nil(t, video.Entries[0].Tenc)
	assert.Equal(t, [4]byte{'c', 'e', 'n', 'c'}, video.Entries[0].SchemeType)
}

func TestReadCencManifestUnknownScheme(t *testing.T) {
	fx := buildCencFixture(t, func(cfg *cencFixtureConfig) {
		cfg.audioScheme = [4]byte{'x', 'y', 'z', ' '}
	})
	m, err := ReadCencManifest(bytes.NewReader(fx.data))
	require.NoError(t, err)
	audio := m.FindTrack(2)
	require.NotNil(t, audio)
	require.Len(t, audio.Entries, 1)
	assert.Equal(t, [4]byte{'x', 'y', 'z', ' '}, audio.Entries[0].SchemeType)
	assert.False(t, audio.Entries[0].SchemeKnown)
}

func TestReadCencManifestErrors(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*cencFixtureConfig)
		target error
	}{
		{
			name: "invalid per-sample IV size",
			mutate: func(cfg *cencFixtureConfig) {
				cfg.videoTenc.DefaultPerSampleIVSize = 7
			},
			target: ErrCencInvalidIVSize,
		},
		{
			name: "invalid constant IV size",
			mutate: func(cfg *cencFixtureConfig) {
				cfg.audioTenc.DefaultConstantIVSize = 4
				cfg.audioTenc.DefaultConstantIV = []byte{0x01, 0x02, 0x03, 0x04}
			},
			target: ErrCencInvalidIVSize,
		},
		{
			name: "saio offset out of range",
			mutate: func(cfg *cencFixtureConfig) {
				cfg.saioDelta = 0x00f00000
			},
			target: ErrCencSaioOffsetOutOfRange,
		},
		{
			name: "conflicting KIDs for the same system ID",
			mutate: func(cfg *cencFixtureConfig) {
				cfg.moofPssh = &Pssh{
					FullBox:  FullBox{Version: 1},
					SystemID: cencSystemIDB,
					KIDCount: 1,
					KIDs:     []PsshKID{{KID: cencAudioKID}},
				}
			},
			target: ErrCencConflictingKID,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fx := buildCencFixture(t, tc.mutate)
			_, err := ReadCencManifest(bytes.NewReader(fx.data))
			require.Error(t, err)
			assert.True(t, errors.Is(err, tc.target), "expected %v, got %v", tc.target, err)
		})
	}
}

// TestReadCencManifestDoesNotReadMdat ensures that media bytes are never
// touched: a reader which fails on any read inside an mdat payload must not
// break manifest reading.
func TestReadCencManifestDoesNotReadMdat(t *testing.T) {
	fx := buildCencFixture(t, nil)

	// collect mdat payload ranges
	bis, err := ExtractBox(bytes.NewReader(fx.data), nil, BoxPath{BoxTypeMdat()})
	require.NoError(t, err)
	require.Len(t, bis, 2)
	var mdatRanges []cencByteRange
	for _, bi := range bis {
		mdatRanges = append(mdatRanges, cencByteRange{
			start: int64(bi.Offset + bi.HeaderSize),
			end:   int64(bi.Offset + bi.Size),
		})
	}

	r := &cencGuardReader{r: bytes.NewReader(fx.data), forbidden: mdatRanges}
	_, err = ReadCencManifest(r)
	require.NoError(t, err)
}

type cencByteRange struct{ start, end int64 }

type cencGuardReader struct {
	r         *bytes.Reader
	pos       int64
	forbidden []cencByteRange
}

func (g *cencGuardReader) Read(p []byte) (int, error) {
	for _, f := range g.forbidden {
		if g.pos < f.end && g.pos+int64(len(p)) > f.start {
			return 0, errors.New("cencGuardReader: read inside mdat payload")
		}
	}
	n, err := g.r.Read(p)
	g.pos += int64(n)
	return n, err
}

func (g *cencGuardReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := g.r.Seek(offset, whence)
	g.pos = pos
	return pos, err
}

var _ io.ReadSeeker = (*cencGuardReader)(nil)
