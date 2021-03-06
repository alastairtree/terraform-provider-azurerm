package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
	"github.com/tombuildsstuff/giovanni/storage/2018-11-09/blob/blobs"
)

const pollingInterval = time.Second * 15

type BlobUpload struct {
	Client *blobs.Client

	AccountName   string
	BlobName      string
	ContainerName string

	BlobType    string
	ContentType string
	MetaData    map[string]string
	Parallelism int
	Size        int
	Source      string
	SourceUri   string
}

func (sbu BlobUpload) Create(ctx context.Context) error {
	if sbu.SourceUri != "" {
		return sbu.copy(ctx)
	}

	blobType := strings.ToLower(sbu.BlobType)

	// TODO: new feature for 'append' blobs?

	if blobType == "block" {
		if sbu.Source != "" {
			return sbu.uploadBlockBlob(ctx)
		}

		return sbu.createEmptyBlockBlob(ctx)
	}

	if blobType == "page" {
		if sbu.Source != "" {
			return sbu.uploadPageBlob(ctx)
		}

		return sbu.createEmptyPageBlob(ctx)
	}

	return fmt.Errorf("Unsupported Blob Type: %q", blobType)
}

func (sbu BlobUpload) copy(ctx context.Context) error {
	input := blobs.CopyInput{
		CopySource: sbu.SourceUri,
		MetaData:   sbu.MetaData,
	}
	if err := sbu.Client.CopyAndWait(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, input, pollingInterval); err != nil {
		return fmt.Errorf("Error copy/waiting: %s", err)
	}

	return nil
}

func (sbu BlobUpload) createEmptyBlockBlob(ctx context.Context) error {
	input := blobs.PutBlockBlobInput{
		ContentType: utils.String(sbu.ContentType),
		MetaData:    sbu.MetaData,
	}
	if _, err := sbu.Client.PutBlockBlob(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, input); err != nil {
		return fmt.Errorf("Error PutBlockBlob: %s", err)
	}

	return nil
}

func (sbu BlobUpload) uploadBlockBlob(ctx context.Context) error {
	file, err := os.Open(sbu.Source)
	if err != nil {
		return fmt.Errorf("Error opening: %s", err)
	}
	defer file.Close()

	input := blobs.PutBlockBlobInput{
		ContentType: utils.String(sbu.ContentType),
		MetaData:    sbu.MetaData,
	}
	if err := sbu.Client.PutBlockBlobFromFile(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, file, input); err != nil {
		return fmt.Errorf("Error PutBlockBlobFromFile: %s", err)
	}

	return nil
}

func (sbu BlobUpload) createEmptyPageBlob(ctx context.Context) error {
	if sbu.Size == 0 {
		return fmt.Errorf("`size` cannot be zero for a page blob")
	}

	input := blobs.PutPageBlobInput{
		BlobContentLengthBytes: int64(sbu.Size),
		ContentType:            utils.String(sbu.ContentType),
		MetaData:               sbu.MetaData,
	}
	if _, err := sbu.Client.PutPageBlob(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, input); err != nil {
		return fmt.Errorf("Error PutPageBlob: %s", err)
	}

	return nil
}

func (sbu BlobUpload) uploadPageBlob(ctx context.Context) error {
	if sbu.Size != 0 {
		return fmt.Errorf("`size` cannot be set for an uploaded page blob")
	}

	// determine the details about the file
	file, err := os.Open(sbu.Source)
	if err != nil {
		return fmt.Errorf("Error opening source file for upload %q: %s", sbu.Source, err)
	}
	defer file.Close()

	// TODO: all of this ultimately can be moved into Giovanni

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("Could not stat file %q: %s", file.Name(), err)
	}

	fileSize := info.Size()

	// first let's create a file of the specified file size
	input := blobs.PutPageBlobInput{
		BlobContentLengthBytes: fileSize,
		ContentType:            utils.String(sbu.ContentType),
		MetaData:               sbu.MetaData,
	}
	// TODO: access tiers?
	if _, err := sbu.Client.PutPageBlob(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, input); err != nil {
		return fmt.Errorf("Error PutPageBlob: %s", err)
	}

	if err := sbu.pageUploadFromSource(ctx, file, fileSize); err != nil {
		return fmt.Errorf("Error creating storage blob on Azure: %s", err)
	}

	return nil
}

// TODO: move below here into Giovanni

type storageBlobPage struct {
	offset  int64
	section *io.SectionReader
}

func (sbu BlobUpload) pageUploadFromSource(ctx context.Context, file io.ReaderAt, fileSize int64) error {
	workerCount := sbu.Parallelism * runtime.NumCPU()

	// first we chunk the file and assign them to 'pages'
	pageList, err := sbu.storageBlobPageSplit(file, fileSize)
	if err != nil {
		return fmt.Errorf("Error splitting source file %q into pages: %s", sbu.Source, err)
	}

	// finally we upload the contents of said file
	pages := make(chan storageBlobPage, len(pageList))
	errors := make(chan error, len(pageList))
	wg := &sync.WaitGroup{}
	wg.Add(len(pageList))

	total := int64(0)
	for _, page := range pageList {
		total += page.section.Size()
		pages <- page
	}
	close(pages)

	for i := 0; i < workerCount; i++ {
		go sbu.blobPageUploadWorker(ctx, blobPageUploadContext{
			blobSize: fileSize,
			pages:    pages,
			errors:   errors,
			wg:       wg,
		})
	}

	wg.Wait()

	if len(errors) > 0 {
		return fmt.Errorf("Error while uploading source file %q: %s", sbu.Source, <-errors)
	}

	return nil
}

const (
	minPageSize int64 = 4 * 1024

	// TODO: investigate whether this can be bumped to 100MB with the new API
	maxPageSize int64 = 4 * 1024 * 1024
)

func (sbu BlobUpload) storageBlobPageSplit(file io.ReaderAt, fileSize int64) ([]storageBlobPage, error) {
	// whilst the file Size can be any arbitrary Size, it must be uploaded in fixed-Size pages
	blobSize := fileSize
	if fileSize%minPageSize != 0 {
		blobSize = fileSize + (minPageSize - (fileSize % minPageSize))
	}

	emptyPage := make([]byte, minPageSize)

	type byteRange struct {
		offset int64
		length int64
	}

	var nonEmptyRanges []byteRange
	var currentRange byteRange
	for i := int64(0); i < blobSize; i += minPageSize {
		pageBuf := make([]byte, minPageSize)
		if _, err := file.ReadAt(pageBuf, i); err != nil && err != io.EOF {
			return nil, fmt.Errorf("Could not read chunk at %d: %s", i, err)
		}

		if bytes.Equal(pageBuf, emptyPage) {
			if currentRange.length != 0 {
				nonEmptyRanges = append(nonEmptyRanges, currentRange)
			}
			currentRange = byteRange{
				offset: i + minPageSize,
			}
		} else {
			currentRange.length += minPageSize
			if currentRange.length == maxPageSize || (currentRange.offset+currentRange.length == blobSize) {
				nonEmptyRanges = append(nonEmptyRanges, currentRange)
				currentRange = byteRange{
					offset: i + minPageSize,
				}
			}
		}
	}

	var pages []storageBlobPage
	for _, nonEmptyRange := range nonEmptyRanges {
		pages = append(pages, storageBlobPage{
			offset:  nonEmptyRange.offset,
			section: io.NewSectionReader(file, nonEmptyRange.offset, nonEmptyRange.length),
		})
	}

	return pages, nil
}

type blobPageUploadContext struct {
	blobSize int64
	pages    chan storageBlobPage
	errors   chan error
	wg       *sync.WaitGroup
}

func (sbu BlobUpload) blobPageUploadWorker(ctx context.Context, uploadCtx blobPageUploadContext) {
	for page := range uploadCtx.pages {
		start := page.offset
		end := page.offset + page.section.Size() - 1
		if end > uploadCtx.blobSize-1 {
			end = uploadCtx.blobSize - 1
		}
		size := end - start + 1

		chunk := make([]byte, size)
		if _, err := page.section.Read(chunk); err != nil && err != io.EOF {
			uploadCtx.errors <- fmt.Errorf("Error reading source file %q at offset %d: %s", sbu.Source, page.offset, err)
			uploadCtx.wg.Done()
			continue
		}

		input := blobs.PutPageUpdateInput{
			StartByte: start,
			EndByte:   end,
			Content:   chunk,
		}

		if _, err := sbu.Client.PutPageUpdate(ctx, sbu.AccountName, sbu.ContainerName, sbu.BlobName, input); err != nil {
			uploadCtx.errors <- fmt.Errorf("Error writing page at offset %d for file %q: %s", page.offset, sbu.Source, err)
			uploadCtx.wg.Done()
			continue
		}

		uploadCtx.wg.Done()
	}
}
