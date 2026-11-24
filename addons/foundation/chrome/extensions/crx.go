package extensions

import (
	"archive/zip"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/mediabuyerbot/go-crx3/pb"
	"google.golang.org/protobuf/proto"
)

const (
	crxPrefixSize       = 12
	crxFormatVersion    = 3
	maxCRXHeaderBytes   = 4 << 20
	crxSignatureContext = "CRX3 SignedData\x00"
)

// inspectCRX checks the declared ID and reads the version from the same ZIP
// bytes authenticated by the configured digest or an ID-bound developer proof.
// Chromium still verifies all proofs, including any publisher proof it requires
// for a particular install source, and enforces extension install policy.
// Format: https://chromium.googlesource.com/chromium/src/+/879b13b2715914eb6263fbbfa1eae7e1d49f9c1c/components/crx_file/crx_verifier.cc#155
func inspectCRX(path, id string, requireProof bool) (_ string, err error) {
	file, size, err := openBoundedFile(path, maxArtifactBytes)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	header, offset, err := readCRXHeader(file, size, id)
	if err != nil {
		return "", fmt.Errorf("read CRX extension ID from %s: %w", path, err)
	}
	archive := io.NewSectionReader(file, offset, size-offset)
	if requireProof {
		if err := verifyCRXDeveloperProof(header, id, archive); err != nil {
			return "", fmt.Errorf("verify CRX developer proof for %s: %w", id, err)
		}
	}
	version, err := crxManifestVersion(archive)
	if err != nil {
		return "", fmt.Errorf("read CRX manifest for %s: %w", id, err)
	}
	return version, nil
}

func verifyCRXID(path, want string) (err error) {
	file, size, err := openBoundedFile(path, maxArtifactBytes)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, _, err := readCRXHeader(file, size, want); err != nil {
		return fmt.Errorf("read CRX extension ID from %s: %w", path, err)
	}
	return nil
}

func readCRXHeader(reader io.Reader, size int64, want string) (*pb.CrxFileHeader, int64, error) {
	var prefix [crxPrefixSize]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, 0, err
	}
	if string(prefix[:4]) != "Cr24" || binary.LittleEndian.Uint32(prefix[4:8]) != crxFormatVersion {
		return nil, 0, errors.New("not a full CRX3 file")
	}
	headerSize := int64(binary.LittleEndian.Uint32(prefix[8:]))
	if headerSize == 0 || headerSize > maxCRXHeaderBytes || headerSize >= size-crxPrefixSize {
		return nil, 0, fmt.Errorf("invalid CRX3 header size %d", headerSize)
	}
	data := make([]byte, headerSize)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, 0, err
	}
	var header pb.CrxFileHeader
	if err := proto.Unmarshal(data, &header); err != nil {
		return nil, 0, err
	}
	var signed pb.SignedData
	if err := proto.Unmarshal(header.GetSignedHeaderData(), &signed); err != nil {
		return nil, 0, err
	}
	if id := signed.GetCrxId(); len(id) != extensionIDLength/extensionIDCharactersPerByte {
		return nil, 0, fmt.Errorf("invalid CRX3 ID size %d", len(id))
	} else if got := extensionIDFromHashPrefix(id); got != want {
		return nil, 0, fmt.Errorf("declared ID %s does not match %s", got, want)
	}
	return &header, crxPrefixSize + headerSize, nil
}

func verifyCRXDeveloperProof(header *pb.CrxFileHeader, id string, archive *io.SectionReader) error {
	digest := sha256.New()
	signed := header.GetSignedHeaderData()
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(signed)))
	_, _ = digest.Write([]byte(crxSignatureContext))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(signed)
	if n, err := io.Copy(digest, archive); err != nil {
		return err
	} else if n != archive.Size() {
		return io.ErrUnexpectedEOF
	}
	var sum [sha256.Size]byte
	checksum := digest.Sum(sum[:0])
	for _, proof := range header.GetSha256WithRsa() {
		key, ok := crxDeveloperKey(proof, id).(*rsa.PublicKey)
		if ok && rsa.VerifyPKCS1v15(key, crypto.SHA256, checksum, proof.GetSignature()) == nil {
			return nil
		}
	}
	for _, proof := range header.GetSha256WithEcdsa() {
		key, ok := crxDeveloperKey(proof, id).(*ecdsa.PublicKey)
		if ok && ecdsa.VerifyASN1(key, checksum, proof.GetSignature()) {
			return nil
		}
	}
	return errors.New("no valid signature by the declared extension's developer key")
}

func crxDeveloperKey(proof *pb.AsymmetricKeyProof, id string) any {
	data := proof.GetPublicKey()
	if extensionIDFromBytes(data) != id {
		return nil
	}
	key, err := x509.ParsePKIXPublicKey(data)
	if err != nil {
		return nil
	}
	return key
}

func crxManifestVersion(region *io.SectionReader) (string, error) {
	archive, err := openZIP(region, region.Size())
	if err != nil {
		return "", err
	}
	var manifest *zip.File
	if err := visitArchiveEntries(archive, func(entry *zip.File, name string) error {
		if name == manifestFilename && entry.Mode().IsRegular() {
			manifest = entry
		}
		return nil
	}); err != nil {
		return "", err
	}
	if manifest == nil || manifest.UncompressedSize64 > maxManifestBytes {
		return "", errors.New("missing or oversized manifest.json")
	}
	body, err := manifest.Open()
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxManifestBytes+1))
	if err := errors.Join(err, body.Close()); err != nil {
		return "", err
	}
	if len(data) > maxManifestBytes {
		return "", errors.New("oversized manifest.json")
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", err
	}
	if !ValidExternalVersion(metadata.Version) {
		return "", fmt.Errorf("invalid extension version %q", metadata.Version)
	}
	return metadata.Version, nil
}
