package main

import (
	"bytes"
	"compress/bzip2"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"github.com/valyala/gozstd"

	humanize "github.com/dustin/go-humanize"
	xz "github.com/spencercw/go-xz"
	"github.com/vbauerster/mpb/v5"
	"github.com/vbauerster/mpb/v5/decor"
	"google.golang.org/protobuf/proto"

	"github.com/ssut/payload-dumper-go/chromeos_update_engine"
)

type request struct {
	partition       *chromeos_update_engine.PartitionUpdate
	sourceDirectory string
	targetDirectory string
}

// Payload is a new format for the Android OTA/Firmware update files since Android Oreo
type Payload struct {
	Filename string

	file                 *os.File
	header               *payloadHeader
	deltaArchiveManifest *chromeos_update_engine.DeltaArchiveManifest
	signatures           *chromeos_update_engine.Signatures

	concurrency int

	metadataSize int64
	dataOffset   int64
	initialized  bool

	requests chan *request
	workerWG sync.WaitGroup
	progress *mpb.Progress
}

const (
	payloadHeaderMagic        = "CrAU"
	brilloMajorPayloadVersion = 2
	blockSize                 = 4096
)

type payloadHeader struct {
	Version              uint64
	ManifestLen          uint64
	MetadataSignatureLen uint32
	Size                 uint64

	payload *Payload
}

func (ph *payloadHeader) ReadFromPayload() error {
	buf := make([]byte, 4)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	if string(buf) != payloadHeaderMagic {
		return fmt.Errorf("Invalid payload magic: %s", buf)
	}

	// Read Version
	buf = make([]byte, 8)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.Version = binary.BigEndian.Uint64(buf)
	fmt.Printf("Payload Version: %d\n", ph.Version)

	if ph.Version != brilloMajorPayloadVersion {
		return fmt.Errorf("Unsupported payload version: %d", ph.Version)
	}

	// Read Manifest Len
	buf = make([]byte, 8)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.ManifestLen = binary.BigEndian.Uint64(buf)
	fmt.Printf("Payload Manifest Length: %d\n", ph.ManifestLen)

	ph.Size = 24

	// Read Manifest Signature Length
	buf = make([]byte, 4)
	if _, err := ph.payload.file.Read(buf); err != nil {
		return err
	}
	ph.MetadataSignatureLen = binary.BigEndian.Uint32(buf)
	fmt.Printf("Payload Manifest Signature Length: %d\n", ph.MetadataSignatureLen)

	return nil
}

// NewPayload creates a new Payload struct
func NewPayload(filename string) *Payload {
	payload := &Payload{
		Filename:    filename,
		concurrency: 4,
	}

	return payload
}

// SetConcurrency sets number of workers
func (p *Payload) SetConcurrency(concurrency int) {
	p.concurrency = concurrency
}

// GetConcurrency returns number of workers
func (p *Payload) GetConcurrency() int {
	return p.concurrency
}

// Open tries to open payload.bin file defined by Filename
func (p *Payload) Open() error {
	file, err := os.Open(p.Filename)
	if err != nil {
		return err
	}

	p.file = file
	return nil
}

func (p *Payload) readManifest() (*chromeos_update_engine.DeltaArchiveManifest, error) {
	buf := make([]byte, p.header.ManifestLen)
	if _, err := p.file.Read(buf); err != nil {
		return nil, err
	}
	deltaArchiveManifest := &chromeos_update_engine.DeltaArchiveManifest{}
	if err := proto.Unmarshal(buf, deltaArchiveManifest); err != nil {
		return nil, err
	}

	return deltaArchiveManifest, nil
}

func (p *Payload) readMetadataSignature() (*chromeos_update_engine.Signatures, error) {
	if _, err := p.file.Seek(int64(p.header.Size+p.header.ManifestLen), 0); err != nil {
		return nil, err
	}

	buf := make([]byte, p.header.MetadataSignatureLen)
	if _, err := p.file.Read(buf); err != nil {
		return nil, err
	}
	signatures := &chromeos_update_engine.Signatures{}
	if err := proto.Unmarshal(buf, signatures); err != nil {
		return nil, err
	}

	return signatures, nil
}

func (p *Payload) Init() error {
	// Read Header
	p.header = &payloadHeader{
		payload: p,
	}
	if err := p.header.ReadFromPayload(); err != nil {
		return err
	}

	// Read Manifest
	deltaArchiveManifest, err := p.readManifest()
	if err != nil {
		return err
	}
	p.deltaArchiveManifest = deltaArchiveManifest

	fmt.Printf("Payload Minor Version: %d\n", deltaArchiveManifest.GetMinorVersion())

	// Read Signatures
	signatures, err := p.readMetadataSignature()
	if err != nil {
		return err
	}
	p.signatures = signatures

	// Update sizes
	p.metadataSize = int64(p.header.Size + p.header.ManifestLen)
	p.dataOffset = p.metadataSize + int64(p.header.MetadataSignatureLen)

	fmt.Println("Found partitions:")
	for i, partition := range p.deltaArchiveManifest.Partitions {
		fmt.Printf("%s (%s)", partition.GetPartitionName(), humanize.Bytes(*partition.GetNewPartitionInfo().Size))

		if i < len(deltaArchiveManifest.Partitions)-1 {
			fmt.Printf(", ")
		} else {
			fmt.Printf("\n")
		}
	}
	p.initialized = true

	return nil
}

func (p *Payload) readDataBlob(offset int64, length int64) ([]byte, error) {
	buf := make([]byte, length)
	n, err := p.file.ReadAt(buf, p.dataOffset+offset)
	if err != nil {
		return nil, err
	}
	if int64(n) != length {
		return nil, fmt.Errorf("Read length mismatch: %d != %d", n, length)
	}

	return buf, nil
}

func (p *Payload) Extract(partition *chromeos_update_engine.PartitionUpdate, out *os.File, in *os.File) error {
	name := partition.GetPartitionName()
	info := partition.GetNewPartitionInfo()
	isDelta := in != nil
	totalOperations := len(partition.Operations)
	barName := fmt.Sprintf("%s (%s)", name, humanize.Bytes(info.GetSize()))
	bar := p.progress.AddBar(
		int64(totalOperations),
		mpb.PrependDecorators(
			decor.Name(barName, decor.WCSyncSpaceR),
		),
		mpb.AppendDecorators(
			decor.Percentage(),
		),
	)
	defer bar.SetTotal(0, true)

	for _, operation := range partition.Operations {
		if len(operation.DstExtents) == 0 {
			return fmt.Errorf("Invalid operation.DstExtents for the partition %s", name)
		}
		bar.Increment()

		e := operation.DstExtents[0]
		dataOffset := p.dataOffset + int64(operation.GetDataOffset())
		dataLength := int64(operation.GetDataLength())
		_, err := out.Seek(int64(e.GetStartBlock())*blockSize, 0)
		if err != nil {
			return err
		}
		expectedUncompressedBlockSize := int64(e.GetNumBlocks() * blockSize)

		bufSha := sha256.New()
		teeReader := io.TeeReader(io.NewSectionReader(p.file, dataOffset, dataLength), bufSha)

		dataBuf := make([]byte, dataLength)

		teeReader.Read(dataBuf)

		switch operation.GetType() {
		case chromeos_update_engine.InstallOperation_REPLACE:
			n, err := io.Copy(out, teeReader)
			if err != nil {
				return err
			}

			if int64(n) != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		case chromeos_update_engine.InstallOperation_REPLACE_XZ:
			reader := xz.NewDecompressionReader(teeReader)
			n, err := io.Copy(out, &reader)
			if err != nil {
				return err
			}
			reader.Close()
			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}

			break

		case chromeos_update_engine.InstallOperation_REPLACE_BZ:
			reader := bzip2.NewReader(teeReader)
			n, err := io.Copy(out, reader)
			if err != nil {
				return err
			}
			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		case chromeos_update_engine.InstallOperation_ZSTD:
			reader := gozstd.NewReader(teeReader)
			n, err := io.Copy(out, reader)
			if err != nil {
				return err
			}
			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		case chromeos_update_engine.InstallOperation_ZERO:
			reader := bytes.NewReader(make([]byte, expectedUncompressedBlockSize))
			n, err := io.Copy(out, reader)
			if err != nil {
				return err
			}

			if n != expectedUncompressedBlockSize {
				return fmt.Errorf("Verify failed (Unexpected bytes written): %s (%d != %d)", name, n, expectedUncompressedBlockSize)
			}
			break

		case chromeos_update_engine.InstallOperation_SOURCE_COPY:
			if !isDelta {
				return fmt.Errorf("%s: SOURCE_COPY is only supported for delta", name)
			}

			for _, e := range operation.SrcExtents {
				_, err := in.Seek(int64(e.GetStartBlock()) * blockSize, 0)
				if err != nil {
					return err
				}

				expectedInputBlockSize := int64(e.GetNumBlocks()) * blockSize

				data := make([]byte, expectedInputBlockSize)
				n, err := in.Read(data)

				if err != nil {
					fmt.Printf("%s: SOURCE_COPY error: %s (read %d)\n", name, err, n)
					return err
				}

				if int64(n) != expectedInputBlockSize {
					return fmt.Errorf("%s: SOURCE_COPY expected %d bytes, but got %d", name, expectedInputBlockSize, n)
				}

				if _, err := out.Write(data[:n]); err != nil {
					return err
				}
			}
			break

		case chromeos_update_engine.InstallOperation_SOURCE_BSDIFF:
		case chromeos_update_engine.InstallOperation_BSDIFF:
		case chromeos_update_engine.InstallOperation_BROTLI_BSDIFF:
			if !isDelta {
				return fmt.Errorf("%s: %s is only supported for delta", name, operation.GetType().String())
			}

			buf := make([]byte, 0)

			for _, e := range operation.SrcExtents {
				_, err := in.Seek(int64(e.GetStartBlock()) * blockSize, 0)
				if err != nil {
					return err
				}

				expectedInputBlockSize := int64(e.GetNumBlocks()) * blockSize

				data := make([]byte, expectedInputBlockSize)
				n, err := in.Read(data)

				if err != nil {
					fmt.Printf("%s: %s error: %s (read %d)\n", name, operation.GetType().String(), err, n)
					return err
				}

				if int64(n) != expectedInputBlockSize {
					return fmt.Errorf("%s: %s expected %d bytes, but got %d", name, operation.GetType().String(), expectedInputBlockSize, n)
				}

				buf = append(buf, data...)
			}

			buf, err := chromeos_update_engine.ExecuteSourceBsdiffOperation(buf, dataBuf)

			if err != nil {
				return err
			}

			n := uint64(0)

			for _, e := range operation.DstExtents {
				_, err := out.Seek(int64(e.GetStartBlock())*blockSize, 0)
				if err != nil {
					return err
				}

				data := make([]byte, e.GetNumBlocks() * blockSize)
				copy(data, buf[n*blockSize:])
				if _, err := out.Write(data); err != nil {
					return err
				}
				n += e.GetNumBlocks()
			}
			break
		case chromeos_update_engine.InstallOperation_PUFFDIFF:
			if !isDelta {
				return fmt.Errorf("%s: %s is only supported for delta", name, operation.GetType().String())
			}

			buf := make([]byte, 0)

			for _, e := range operation.SrcExtents {
				_, err := in.Seek(int64(e.GetStartBlock()) * blockSize, 0)
				if err != nil {
					return err
				}

				expectedInputBlockSize := int64(e.GetNumBlocks()) * blockSize

				data := make([]byte, expectedInputBlockSize)
				n, err := in.Read(data)

				if err != nil {
					fmt.Printf("%s: %s error: %s (read %d)\n", name, operation.GetType().String(), err, n)
					return err
				}

				if int64(n) != expectedInputBlockSize {
					return fmt.Errorf("%s: %s expected %d bytes, but got %d", name, operation.GetType().String(), expectedInputBlockSize, n)
				}

				buf = append(buf, data...)
			}

			buf, err := chromeos_update_engine.ExecuteSourcePuffDiffOperation(buf, dataBuf)

			if err != nil {
				return err
			}

			n := uint64(0)

			for _, e := range operation.DstExtents {
				_, err := out.Seek(int64(e.GetStartBlock())*blockSize, 0)
				if err != nil {
					return err
				}

				data := make([]byte, e.GetNumBlocks() * blockSize)
				copy(data, buf[n*blockSize:])
				if _, err := out.Write(data); err != nil {
					return err
				}
				n += e.GetNumBlocks()
			}
			break
			return fmt.Errorf("%s: PUFFDIFF is not yet implemented!", name)

		default:
			return fmt.Errorf("%s: Unhandled operation type: %s", name, operation.GetType().String())
		}

		// verify hash
		hash := hex.EncodeToString(bufSha.Sum(nil))
		expectedHash := hex.EncodeToString(operation.GetDataSha256Hash())
		if expectedHash != "" && hash != expectedHash {
			return fmt.Errorf("Verify failed (Checksum mismatch): %s (%s != %s)", name, hash, expectedHash)
		}
	}

	return nil
}

func (p *Payload) worker() {
	for req := range p.requests {
		partition := req.partition
		targetDirectory := req.targetDirectory
		sourceDirectory := req.sourceDirectory
		isDelta := sourceDirectory != ""

		name := fmt.Sprintf("%s.img", partition.GetPartitionName())
		filepath := fmt.Sprintf("%s/%s", targetDirectory, name)
		file, err := os.OpenFile(filepath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
		}


		sourcepath := fmt.Sprintf("%s/%s", sourceDirectory, name)
		sourcefile, err := os.OpenFile(sourcepath, os.O_RDONLY, 0755)
		if isDelta {
			if err != nil {
				fmt.Println(err.Error())
			}
		} else {
			sourcefile = nil
		}


		if err := p.Extract(partition, file, sourcefile); err != nil {
			fmt.Println(err.Error())
		}

		p.workerWG.Done()
	}
}

func (p *Payload) spawnExtractWorkers(n int) {
	for i := 0; i < n; i++ {
		go p.worker()
	}
}

func (p *Payload) ExtractSelected(sourceDirectory string, targetDirectory string, partitions []string) error {
	if !p.initialized {
		return errors.New("Payload has not been initialized")
	}
	p.progress = mpb.New()

	p.requests = make(chan *request, 100)
	p.spawnExtractWorkers(p.concurrency)

	sort.Strings(partitions)

	for _, partition := range p.deltaArchiveManifest.Partitions {
		if len(partitions) > 0 {
			idx := sort.SearchStrings(partitions, *partition.PartitionName)
			if idx == len(partitions) || partitions[idx] != *partition.PartitionName {
				continue
			}
		}

		p.workerWG.Add(1)
		p.requests <- &request{
			partition:       partition,
			sourceDirectory: sourceDirectory,
			targetDirectory: targetDirectory,
		}
	}

	p.workerWG.Wait()
	close(p.requests)

	return nil
}

func (p *Payload) ExtractAll(sourceDirectory string, targetDirectory string) error {
	return p.ExtractSelected(sourceDirectory, targetDirectory, nil)
}
