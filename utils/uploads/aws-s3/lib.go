package aws_s3

import (
	"bytes"
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const (
	maxRetries = 3
	partSize   = 6000000
)

type fileByteStreamDTO struct {
	err  error
	data []byte
}

func (u *Uploader) fileByteStreamGenerator(inputFilePath string, abort chan interface{}) <-chan fileByteStreamDTO {
	dataByteStream := make(chan fileByteStreamDTO)
	go func() {
		defer close(dataByteStream)
		file, err := os.Open(inputFilePath)
		if err != nil {
			dataByteStream <- fileByteStreamDTO{
				err:  err,
				data: nil,
			}
			return
		}
		defer file.Close()
		totalBytesRead := 0
		aggregateBytesRead := 0
		aggregateBytesBuffer := make([]byte, 0, 2*partSize)
		bytesBuffer := make([]byte, 1000000)
		for {
			nBytesRead, fileReadErr := file.Read(bytesBuffer)
			aggregateBytesRead += nBytesRead
			aggregateBytesBuffer = append(aggregateBytesBuffer, bytesBuffer[:nBytesRead]...)
			if fileReadErr != nil {
				if fileReadErr == io.EOF {
					if aggregateBytesRead > 0 {
						select {
						case <-abort:
							return
						case dataByteStream <- fileByteStreamDTO{
							err:  nil,
							data: aggregateBytesBuffer[:aggregateBytesRead],
						}:
							totalBytesRead += aggregateBytesRead
							aggregateBytesRead = 0
						}
					}
					return
				}

				select {
				case <-abort:
					return
				case dataByteStream <- fileByteStreamDTO{
					err:  fileReadErr,
					data: nil,
				}:
					totalBytesRead += aggregateBytesRead
					aggregateBytesRead = 0
				}
				return
			}

			if aggregateBytesRead >= partSize {
				select {
				case <-abort:
					return
				case dataByteStream <- fileByteStreamDTO{
					err:  nil,
					data: aggregateBytesBuffer[:aggregateBytesRead],
				}:
					totalBytesRead += aggregateBytesRead
					aggregateBytesBuffer = nil
					aggregateBytesRead = 0
				}
			}
		}
	}()
	return dataByteStream
}

func completeMultipartUpload(client *s3.Client, resp *s3.CreateMultipartUploadOutput, completedParts []types.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
	completeInput := &s3.CompleteMultipartUploadInput{
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		UploadId: resp.UploadId,

		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	}
	return client.CompleteMultipartUpload(context.TODO(), completeInput)
}

func uploadPart(client *s3.Client, resp *s3.CreateMultipartUploadOutput, fileBytes []byte, partNumber int) (types.CompletedPart, error) {
	tryNum := 1
	uploadPartInput := &s3.UploadPartInput{
		Body:          bytes.NewReader(fileBytes),
		Bucket:        resp.Bucket,
		Key:           resp.Key,
		PartNumber:    int32(partNumber),
		UploadId:      resp.UploadId,
		ContentLength: int64(len(fileBytes)),
	}

	for tryNum <= maxRetries {
		uploadResult, err := client.UploadPart(context.TODO(), uploadPartInput)
		if err != nil {
			if tryNum == maxRetries {
				//if aerr, ok := err.(awserr.Error); ok {
				//	return nil, aerr
				//}
				return types.CompletedPart{}, err
			}
			//fmt.Printf("Retrying to upload part #%v\n", partNumber)
			tryNum++
		} else {
			//fmt.Printf("Uploaded part #%v\n", partNumber)
			return types.CompletedPart{
				ETag:       uploadResult.ETag,
				PartNumber: int32(partNumber),
			}, nil
		}
	}
	return types.CompletedPart{}, nil
}

func abortMultipartUpload(client *s3.Client, resp *s3.CreateMultipartUploadOutput) error {
	//fmt.Println("Aborting multipart upload for UploadId#" + *resp.UploadId)
	abortInput := &s3.AbortMultipartUploadInput{
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		UploadId: resp.UploadId,
	}
	_, err := client.AbortMultipartUpload(context.TODO(), abortInput)
	return err
}

type uploadFileDoneDTO struct {
	err       error
	done      bool
	objectKey string
}

func getContentTypeMetaTagForFile(filePath, fileContentType string) string {
	var contentTypeData = map[string]string{
		".aac":    "audio/aac",
		".abw":    "application/x-abiword",
		".arc":    "application/x-freearc",
		".avif":   "image/avif",
		".avi":    "video/x-msvideo",
		".azw":    "application/vnd.amazon.ebook",
		".bin":    "application/octet-stream",
		".bmp":    "image/bmp",
		".bz":     "application/x-bzip",
		".bz2":    "application/x-bzip2",
		".cda":    "application/x-cdf",
		".csh":    "application/x-csh",
		".css":    "text/css",
		".csv":    "text/csv",
		".doc":    "application/msword",
		".docx":   "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".eot":    "application/vnd.ms-fontobject",
		".epub":   "application/epub+zip",
		".gz":     "application/gzip",
		".gif":    "image/gif",
		".htm":    "text/html",
		".html":   "text/html",
		".ico":    "image/vnd.microsoft.icon",
		".ics":    "text/calendar",
		".jar":    "application/java-archive",
		".jpeg":   "image/jpeg",
		".jpg":    "image/jpeg",
		".js":     "text/javascript",
		".json":   "application/json",
		".jsonld": "application/ld+json",
		".mid":    "audio/midi",
		".midi":   "audio/midi",
		".mjs":    "text/javascript",
		".mp3":    "audio/mpeg",
		".mp4":    "video/mp4",
		".mpeg":   "video/mpeg",
		".mpkg":   "application/vnd.apple.installer+xml",
		".odp":    "application/vnd.oasis.opendocument.presentation",
		".ods":    "application/vnd.oasis.opendocument.spreadsheet",
		".odt":    "application/vnd.oasis.opendocument.text",
		".oga":    "audio/ogg",
		".ogv":    "video/ogg",
		".ogx":    "application/ogg",
		".opus":   "audio/opus",
		".otf":    "font/otf",
		".png":    "image/png",
		".pdf":    "application/pdf",
		".php":    "application/x-httpd-php",
		".ppt":    "application/vnd.ms-powerpoint",
		".pptx":   "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".rar":    "application/vnd.rar",
		".rtf":    "application/rtf",
		".sh":     "application/x-sh",
		".svg":    "image/svg+xml",
		".tar":    "application/x-tar",
		".tif":    "image/tiff",
		".tiff":   "image/tiff",
		".ts":     "video/mp2t",
		".ttf":    "font/ttf",
		".txt":    "text/plain",
		".vsd":    "application/vnd.visio",
		".wav":    "audio/wav",
		".weba":   "audio/webm",
		".webm":   "video/webm",
		".webp":   "image/webp",
		".woff":   "font/woff",
		".woff2":  "font/woff2",
		".xhtml":  "application/xhtml+xml",
		".xls":    "application/vnd.ms-excel",
		".xlsx":   "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".xml":    "application/xml",
		".xul":    "application/vnd.mozilla.xul+xml",
		".zip":    "application/zip",
		".7z":     "application/x-7z-compressed",
	}

	extension := filepath.Ext(filePath)

	contentType, exists := contentTypeData[extension]
	if !exists {
		return fileContentType
	}

	return contentType

}

func (u *Uploader) uploadByteStreamToS3(filePath, outputS3ObjectKey string, dataByteStream <-chan fileByteStreamDTO,
	abort chan interface{}) <-chan uploadFileDoneDTO {
	fileDoneStream := make(chan uploadFileDoneDTO)
	go func() {
		defer close(fileDoneStream)
		cfg, err := config.LoadDefaultConfig(context.TODO())
		if err != nil {
			fileDoneStream <- uploadFileDoneDTO{
				err:       err,
				done:      false,
				objectKey: outputS3ObjectKey,
			}
			return
		}

		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.Region = u.s3Region
		})

		f, err := os.Open(filePath)
		if err != nil {
			fileDoneStream <- uploadFileDoneDTO{
				err:       err,
				done:      false,
				objectKey: outputS3ObjectKey,
			}
			return
		}

		// Get the content
		fileContentType, err := getFileContentType(f)
		if err != nil {
			fileDoneStream <- uploadFileDoneDTO{
				err:       err,
				done:      false,
				objectKey: outputS3ObjectKey,
			}
			return
		}

		input := &s3.CreateMultipartUploadInput{
			Bucket:      aws.String(u.s3Bucket),
			Key:         aws.String(outputS3ObjectKey),
			ContentType: aws.String(getContentTypeMetaTagForFile(filePath, fileContentType)),
		}

		f.Close()

		resp, err := client.CreateMultipartUpload(context.TODO(), input)
		if err != nil {
			fileDoneStream <- uploadFileDoneDTO{
				err:       err,
				done:      false,
				objectKey: outputS3ObjectKey,
			}
			return
		}

		var completedParts []types.CompletedPart
		partNumber := 1
		for dataBytes := range dataByteStream {
			if dataBytes.err != nil {

			}
			completedPart, err := uploadPart(client, resp, dataBytes.data, partNumber)
			if err != nil {
				_ = abortMultipartUpload(client, resp)
				fileDoneStream <- uploadFileDoneDTO{
					err:       err,
					done:      false,
					objectKey: outputS3ObjectKey,
				}
				return
			}
			partNumber++
			completedParts = append(completedParts, completedPart)
		}
		if len(completedParts) > 0 {
			_, err := completeMultipartUpload(client, resp, completedParts)
			if err != nil {
				fileDoneStream <- uploadFileDoneDTO{
					err:       err,
					done:      false,
					objectKey: outputS3ObjectKey,
				}
				return
			}
		}
		fileDoneStream <- uploadFileDoneDTO{
			err:       nil,
			done:      true,
			objectKey: outputS3ObjectKey,
		}
		return
	}()
	return fileDoneStream
}

func getFileContentType(out *os.File) (string, error) {

	// Only the first 512 bytes are used to sniff the content type.
	buffer := make([]byte, 512)

	_, err := out.Read(buffer)
	if err != nil {
		return "", err
	}

	// Use the net/http package's handy DectectContentType function. Always returns a valid
	// content-type by returning "application/octet-stream" if no others seemed to match.
	contentType := http.DetectContentType(buffer)

	return contentType, nil
}
