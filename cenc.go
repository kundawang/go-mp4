package mp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Sentinel errors returned by ReadCencManifest. Use errors.Is to match them.
var (
	// ErrCencInvalidIVSize is returned when a tenc box declares an IV size
	// which is not usable for CENC (per-sample IV size must be 0, 8 or 16;
	// a constant IV must be 8 or 16 bytes).
	ErrCencInvalidIVSize = errors.New("mp4: cenc: invalid IV size")

	// ErrCencSaioOffsetOutOfRange is returned when a saio offset resolved
	// against its base data offset points outside the file.
	ErrCencSaioOffsetOutOfRange = errors.New("mp4: cenc: saio offset out of range")

	// ErrCencConflictingKID is returned when two pssh boxes share the same
	// system ID but carry different KID sets.
	ErrCencConflictingKID = errors.New("mp4: cenc: conflicting KIDs for the same PSSH system ID")
)

// CencManifest is a track-oriented summary of every CENC-related declaration
// found in the moov and moof boxes of an MP4 file. It is built from box
// headers and metadata boxes only; media data (mdat payloads and the sample
// auxiliary information itself) is never read or decrypted.
type CencManifest struct {
	// Tracks aggregates the protection declarations of moov per track.
	Tracks []*CencTrack

	// PsshBoxes lists every pssh box occurrence in file order, including
	// duplicates. Each entry keeps its source location so that duplicates
	// remain traceable.
	PsshBoxes []*CencPssh

	// Fragments lists the sample auxiliary information locations of every
	// moof/traf in file order.
	Fragments []*CencFragment
}

// FindTrack returns the manifest entry of the given track ID, or nil.
func (m *CencManifest) FindTrack(trackID uint32) *CencTrack {
	for _, t := range m.Tracks {
		if t.TrackID == trackID {
			return t
		}
	}
	return nil
}

// CencTrack is the per-track summary of the protection scheme declared in
// moov/trak/.../stsd.
type CencTrack struct {
	TrackID uint32

	// Encrypted is true when the track has encrypted sample entries
	// (encv/enca) in its sample description.
	Encrypted bool

	// Entries holds one entry per encrypted sample entry of the track.
	// Sample-level information in moof (see CencFragment) overrides the
	// defaults declared here.
	Entries []*CencSampleEntry
}

// CencSampleEntry summarizes the sinf subtree of one encrypted sample entry.
type CencSampleEntry struct {
	// EntryType is the sample entry fourcc, e.g. "encv" or "enca".
	EntryType [4]byte

	// OriginalFormat is the frma fourcc (the unencrypted format), zero when
	// no frma box is present.
	OriginalFormat [4]byte

	// SchemeType and SchemeVersion come from the schm box; both are zero
	// when no schm box is present.
	SchemeType    [4]byte
	SchemeVersion uint32

	// SchemeKnown reports whether SchemeType is one of the schemes defined
	// by ISO/IEC 23001-7 ("cenc", "cens", "cbc1", "cbcs"). An unknown
	// scheme is reported here instead of failing the whole manifest.
	SchemeKnown bool

	// Tenc holds the track encryption defaults. It is nil when the sample
	// entry has no tenc box, so a missing tenc is visible in the result
	// rather than an error.
	Tenc *CencTenc
}

// CencTenc mirrors the defaults of a tenc box.
type CencTenc struct {
	IsProtected     uint8
	PerSampleIVSize uint8
	KID             [16]byte
	// ConstantIV is the default constant IV; it is only meaningful when
	// PerSampleIVSize is 0.
	ConstantIV     []byte
	CryptByteBlock uint8
	SkipByteBlock  uint8
}

// CencPsshLocation identifies where a pssh box was found.
type CencPsshLocation int

const (
	// CencPsshInMoov indicates a pssh box inside moov.
	CencPsshInMoov CencPsshLocation = iota
	// CencPsshInMoof indicates a pssh box inside a moof box.
	CencPsshInMoof
)

// CencPsshSource keeps the origin of a pssh box so that duplicate boxes
// remain traceable to their location in the file.
type CencPsshSource struct {
	Location  CencPsshLocation
	BoxOffset uint64 // absolute offset of the pssh box in the file
	// MoofOffset is the absolute offset of the enclosing moof box; it is
	// only set when Location is CencPsshInMoof.
	MoofOffset uint64
}

// CencPssh is one pssh box occurrence.
type CencPssh struct {
	Source   CencPsshSource
	SystemID [16]byte
	KIDs     [][16]byte
	DataSize int32
}

// CencFragment locates the sample auxiliary information of one moof/traf.
// The sizes recorded here are the sample-level values; where they are
// absent, the defaults of the track's CencTenc apply.
type CencFragment struct {
	MoofOffset uint64 // absolute offset of the enclosing moof box
	TrackID    uint32 // from tfhd

	// AuxInfoType and AuxInfoTypeParameter come from saio/saiz; they are
	// zero unless the corresponding box flag is set.
	AuxInfoType          [4]byte
	AuxInfoTypeParameter uint32

	// AuxInfoOffsets are the saio offsets resolved to absolute file
	// offsets (base data offset + saio entry). The sample auxiliary
	// information itself is not read.
	AuxInfoOffsets []uint64

	// SampleCount is the saiz sample count.
	SampleCount uint32

	// DefaultSampleInfoSize applies to every sample when SampleInfoSizes
	// is nil.
	DefaultSampleInfoSize uint8

	// SampleInfoSizes holds the per-sample sizes from saiz; it is nil when
	// saiz uses DefaultSampleInfoSize for all samples.
	SampleInfoSizes []uint8

	// SencOffset is the absolute offset of the senc box, or -1 when the
	// traf has none. The senc payload is not parsed.
	SencOffset int64
}

// GetSampleInfoSize returns the auxiliary information size of the sample at
// the given index: the per-sample override when saiz carries one, otherwise
// DefaultSampleInfoSize.
func (f *CencFragment) GetSampleInfoSize(index int) uint8 {
	if f.SampleInfoSizes != nil {
		if index >= 0 && index < len(f.SampleInfoSizes) {
			return f.SampleInfoSizes[index]
		}
		return 0
	}
	return f.DefaultSampleInfoSize
}

// ReadCencManifest reads the CENC manifest of r without reading or
// decrypting any media bytes. Only box headers and the metadata boxes under
// moov and moof are parsed.
func ReadCencManifest(r io.ReadSeeker) (*CencManifest, error) {
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}

	tops, err := ExtractBoxes(r, nil, []BoxPath{
		{BoxTypeMoov()},
		{BoxTypeMoof()},
	})
	if err != nil {
		return nil, err
	}

	manifest := &CencManifest{}
	for _, top := range tops {
		switch top.Type {
		case BoxTypeMoov():
			psshs, err := readCencPsshBoxes(r, top, CencPsshSource{Location: CencPsshInMoov})
			if err != nil {
				return nil, err
			}
			manifest.PsshBoxes = append(manifest.PsshBoxes, psshs...)

			traks, err := ExtractBox(r, top, BoxPath{BoxTypeTrak()})
			if err != nil {
				return nil, err
			}
			for _, trak := range traks {
				track, err := readCencTrack(r, trak)
				if err != nil {
					return nil, err
				}
				manifest.Tracks = append(manifest.Tracks, track)
			}
		case BoxTypeMoof():
			psshs, err := readCencPsshBoxes(r, top, CencPsshSource{Location: CencPsshInMoof, MoofOffset: top.Offset})
			if err != nil {
				return nil, err
			}
			manifest.PsshBoxes = append(manifest.PsshBoxes, psshs...)

			fragments, err := readCencMoof(r, top, uint64(fileSize))
			if err != nil {
				return nil, err
			}
			manifest.Fragments = append(manifest.Fragments, fragments...)
		}
	}

	if err := checkCencPsshConsistency(manifest.PsshBoxes); err != nil {
		return nil, err
	}
	return manifest, nil
}

func readCencPsshBoxes(r io.ReadSeeker, parent *BoxInfo, src CencPsshSource) ([]*CencPssh, error) {
	bips, err := ExtractBoxesWithPayload(r, parent, []BoxPath{{BoxTypePssh()}})
	if err != nil {
		return nil, err
	}
	psshs := make([]*CencPssh, 0, len(bips))
	for _, bip := range bips {
		pssh, ok := bip.Payload.(*Pssh)
		if !ok {
			return nil, fmt.Errorf("mp4: cenc: unexpected payload type %T for pssh box at offset %d", bip.Payload, bip.Info.Offset)
		}
		entry := &CencPssh{
			SystemID: pssh.SystemID,
			DataSize: pssh.DataSize,
		}
		entry.Source = src
		entry.Source.BoxOffset = bip.Info.Offset
		for _, k := range pssh.KIDs {
			entry.KIDs = append(entry.KIDs, k.KID)
		}
		psshs = append(psshs, entry)
	}
	return psshs, nil
}

// checkCencPsshConsistency reports an error when the same system ID appears
// with different KID sets. Identical duplicates are kept as-is in the
// manifest and are not an error.
func checkCencPsshConsistency(psshs []*CencPssh) error {
	kidsBySystemID := make(map[[16]byte][][16]byte)
	for _, pssh := range psshs {
		kids := append([][16]byte(nil), pssh.KIDs...)
		sort.Slice(kids, func(i, j int) bool {
			return bytes.Compare(kids[i][:], kids[j][:]) < 0
		})
		existing, ok := kidsBySystemID[pssh.SystemID]
		if !ok {
			kidsBySystemID[pssh.SystemID] = kids
			continue
		}
		if len(existing) != len(kids) {
			return fmt.Errorf("mp4: cenc: pssh system ID %x at offset %d: %w", pssh.SystemID, pssh.Source.BoxOffset, ErrCencConflictingKID)
		}
		for i := range existing {
			if existing[i] != kids[i] {
				return fmt.Errorf("mp4: cenc: pssh system ID %x at offset %d: %w", pssh.SystemID, pssh.Source.BoxOffset, ErrCencConflictingKID)
			}
		}
	}
	return nil
}

func readCencTrack(r io.ReadSeeker, trak *BoxInfo) (*CencTrack, error) {
	tkhds, err := ExtractBoxWithPayload(r, trak, BoxPath{BoxTypeTkhd()})
	if err != nil {
		return nil, err
	}
	if len(tkhds) == 0 {
		return nil, errors.New("mp4: cenc: tkhd box not found")
	}
	tkhd, ok := tkhds[0].Payload.(*Tkhd)
	if !ok {
		return nil, fmt.Errorf("mp4: cenc: unexpected payload type %T for tkhd box", tkhds[0].Payload)
	}

	track := &CencTrack{TrackID: tkhd.TrackID}

	entries, err := ExtractBoxes(r, trak, []BoxPath{
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEncv()},
		{BoxTypeMdia(), BoxTypeMinf(), BoxTypeStbl(), BoxTypeStsd(), BoxTypeEnca()},
	})
	if err != nil {
		return nil, err
	}
	for _, entryBi := range entries {
		entry, err := readCencSampleEntry(r, entryBi, track.TrackID)
		if err != nil {
			return nil, err
		}
		track.Entries = append(track.Entries, entry)
	}
	track.Encrypted = len(track.Entries) != 0
	return track, nil
}

func readCencSampleEntry(r io.ReadSeeker, entryBi *BoxInfo, trackID uint32) (*CencSampleEntry, error) {
	entry := &CencSampleEntry{}
	copy(entry.EntryType[:], entryBi.Type[:])

	bips, err := ExtractBoxesWithPayload(r, entryBi, []BoxPath{
		{BoxTypeSinf(), BoxTypeFrma()},
		{BoxTypeSinf(), BoxTypeSchm()},
		{BoxTypeSinf(), BoxTypeSchi(), BoxTypeTenc()},
	})
	if err != nil {
		return nil, err
	}
	for _, bip := range bips {
		switch payload := bip.Payload.(type) {
		case *Frma:
			entry.OriginalFormat = payload.DataFormat
		case *Schm:
			entry.SchemeType = payload.SchemeType
			entry.SchemeVersion = payload.SchemeVersion
		case *Tenc:
			tenc, err := convertCencTenc(payload, trackID, bip.Info.Offset)
			if err != nil {
				return nil, err
			}
			entry.Tenc = tenc
		}
	}
	entry.SchemeKnown = isKnownCencScheme(entry.SchemeType)
	return entry, nil
}

func isKnownCencScheme(scheme [4]byte) bool {
	switch scheme {
	case [4]byte{'c', 'e', 'n', 'c'},
		[4]byte{'c', 'e', 'n', 's'},
		[4]byte{'c', 'b', 'c', '1'},
		[4]byte{'c', 'b', 'c', 's'}:
		return true
	}
	return false
}

func convertCencTenc(tenc *Tenc, trackID uint32, offset uint64) (*CencTenc, error) {
	ct := &CencTenc{
		IsProtected:     tenc.DefaultIsProtected,
		PerSampleIVSize: tenc.DefaultPerSampleIVSize,
		KID:             tenc.DefaultKID,
		CryptByteBlock:  tenc.DefaultCryptByteBlock,
		SkipByteBlock:   tenc.DefaultSkipByteBlock,
	}
	if tenc.DefaultIsProtected != 1 {
		return ct, nil
	}
	switch tenc.DefaultPerSampleIVSize {
	case 0:
		if len(tenc.DefaultConstantIV) != 8 && len(tenc.DefaultConstantIV) != 16 {
			return nil, fmt.Errorf("mp4: cenc: track %d: tenc at offset %d: constant IV size %d: %w",
				trackID, offset, len(tenc.DefaultConstantIV), ErrCencInvalidIVSize)
		}
		ct.ConstantIV = append([]byte(nil), tenc.DefaultConstantIV...)
	case 8, 16:
	default:
		return nil, fmt.Errorf("mp4: cenc: track %d: tenc at offset %d: per-sample IV size %d: %w",
			trackID, offset, tenc.DefaultPerSampleIVSize, ErrCencInvalidIVSize)
	}
	return ct, nil
}

func readCencMoof(r io.ReadSeeker, moof *BoxInfo, fileSize uint64) ([]*CencFragment, error) {
	trafs, err := ExtractBox(r, moof, BoxPath{BoxTypeTraf()})
	if err != nil {
		return nil, err
	}
	fragments := make([]*CencFragment, 0, len(trafs))
	for _, traf := range trafs {
		fragment, err := readCencTraf(r, moof, traf, fileSize)
		if err != nil {
			return nil, err
		}
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

func readCencTraf(r io.ReadSeeker, moof, traf *BoxInfo, fileSize uint64) (*CencFragment, error) {
	bips, err := ExtractBoxesWithPayload(r, traf, []BoxPath{
		{BoxTypeTfhd()},
		{BoxTypeSaiz()},
		{BoxTypeSaio()},
	})
	if err != nil {
		return nil, err
	}
	// senc has no registered payload type here; only its location is needed.
	sencs, err := ExtractBox(r, traf, BoxPath{StrToBoxType("senc")})
	if err != nil {
		return nil, err
	}

	fragment := &CencFragment{
		MoofOffset: moof.Offset,
		SencOffset: -1,
	}
	baseDataOffset := moof.Offset
	for _, bip := range bips {
		switch payload := bip.Payload.(type) {
		case *Tfhd:
			fragment.TrackID = payload.TrackID
			if payload.CheckFlag(TfhdBaseDataOffsetPresent) {
				baseDataOffset = payload.BaseDataOffset
			}
		case *Saiz:
			if payload.CheckFlag(0x000001) {
				fragment.AuxInfoType = payload.AuxInfoType
				fragment.AuxInfoTypeParameter = payload.AuxInfoTypeParameter
			}
			fragment.SampleCount = payload.SampleCount
			fragment.DefaultSampleInfoSize = payload.DefaultSampleInfoSize
			if payload.DefaultSampleInfoSize == 0 {
				fragment.SampleInfoSizes = append([]uint8(nil), payload.SampleInfoSize...)
			}
		case *Saio:
			if payload.CheckFlag(0x000001) {
				fragment.AuxInfoType = payload.AuxInfoType
				fragment.AuxInfoTypeParameter = payload.AuxInfoTypeParameter
			}
			for i := uint32(0); i < payload.EntryCount; i++ {
				offset := payload.GetOffset(int(i))
				if offset > ^uint64(0)-baseDataOffset || baseDataOffset+offset >= fileSize {
					return nil, fmt.Errorf("mp4: cenc: track %d: saio entry %d (base 0x%x + offset 0x%x) at moof offset %d: %w",
						fragment.TrackID, i, baseDataOffset, offset, moof.Offset, ErrCencSaioOffsetOutOfRange)
				}
				fragment.AuxInfoOffsets = append(fragment.AuxInfoOffsets, baseDataOffset+offset)
			}
		}
	}
	if len(sencs) != 0 {
		fragment.SencOffset = int64(sencs[0].Offset)
	}
	return fragment, nil
}
