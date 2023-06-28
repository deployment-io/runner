package aws_s3

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Uploader struct {
	s3Client *s3.Client
	s3Region string
	s3Bucket string
}

func NewUploader(s3Region, s3Bucket string, s3Client *s3.Client) (*Uploader, error) {
	return &Uploader{
		s3Client: s3Client,
		s3Region: s3Region,
		s3Bucket: s3Bucket,
	}, nil
}

func isPathDirectory(directoryPath string) (bool, error) {
	fileInfo, err := os.Stat(directoryPath)
	if err != nil {
		// error handling
		return false, err
	}

	if !fileInfo.IsDir() {
		// is not a directory
		return false, nil
	}

	return true, nil
}

// we can assume that this function is called only for directory
func (u *Uploader) uploadDirectory(directoryPath string, logsWriter io.Writer) error {
	fileUploadDoneSignals := make([]<-chan uploadFileDoneDTO, 0)
	abortUploadSignal := make(chan interface{})
	root := directoryPath
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			//	upload if it's not directory
			outputS3ObjectKey := strings.TrimPrefix(path, directoryPath+"/")
			fileUploadDoneSignal, fileUploadErr := u.UploadFile(path, outputS3ObjectKey, abortUploadSignal)
			if fileUploadErr != nil {
				return fileUploadErr
			}
			fileUploadDoneSignals = append(fileUploadDoneSignals, fileUploadDoneSignal)
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, fileUploadDoneSignal := range fileUploadDoneSignals {
		done := <-fileUploadDoneSignal
		if done.done {
			io.WriteString(logsWriter, fmt.Sprintf("Successfully uploaded file: %s\n", done.objectKey))
		} else {
			io.WriteString(logsWriter, fmt.Sprintf("Error uploading file: %s\n", done.objectKey))
		}
		if done.err != nil {
			return err
		}
	}
	return nil
}

var DirectoryErr = fmt.Errorf("path is not a directory path")

func (u *Uploader) UploadDirectory(directoryPath string, logsWriter io.Writer) error {
	isDirectory, err := isPathDirectory(directoryPath)
	if err != nil {
		return err
	}
	if !isDirectory {
		return DirectoryErr
	}
	return u.uploadDirectory(directoryPath, logsWriter)
}

func (u *Uploader) UploadFile(inputFilePath, outputS3ObjectKey string, abortUploadSignal chan interface{}) (<-chan uploadFileDoneDTO, error) {
	fileBytesStream := u.fileByteStreamGenerator(inputFilePath, abortUploadSignal)
	fileUploadDoneSignal := u.uploadByteStreamToS3(inputFilePath, outputS3ObjectKey, fileBytesStream, abortUploadSignal)
	return fileUploadDoneSignal, nil
}
