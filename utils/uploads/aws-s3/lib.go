package aws_s3

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"io"
	"log"
	"net/http"
	"os"
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
			fmt.Printf("Retrying to upload part #%v\n", partNumber)
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
	fmt.Println("Aborting multipart upload for UploadId#" + *resp.UploadId)
	abortInput := &s3.AbortMultipartUploadInput{
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		UploadId: resp.UploadId,
	}
	_, err := client.AbortMultipartUpload(context.TODO(), abortInput)
	return err
}

type uploadFileDoneDTO struct {
	err  error
	done bool
}

func (u *Uploader) uploadByteStreamToS3(filePath, outputS3ObjectKey string, dataByteStream <-chan fileByteStreamDTO,
	abort chan interface{}) <-chan uploadFileDoneDTO {
	fileDoneStream := make(chan uploadFileDoneDTO)
	go func() {
		defer close(fileDoneStream)
		cfg, err := config.LoadDefaultConfig(context.TODO())
		if err != nil {
			log.Fatal(err)
		}

		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.Region = u.s3Region
		})

		f, err := os.Open(filePath)
		if err != nil {
			panic(err)
		}

		// Get the content
		contentType, err := getFileContentType(f)
		if err != nil {
			panic(err)
		}

		input := &s3.CreateMultipartUploadInput{
			Bucket:      aws.String(u.s3Bucket),
			Key:         aws.String(outputS3ObjectKey),
			ContentType: aws.String(contentType),
		}

		f.Close()

		resp, err := client.CreateMultipartUpload(context.TODO(), input)
		if err != nil {
			fileDoneStream <- uploadFileDoneDTO{
				err:  err,
				done: false,
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
				fmt.Println(err.Error())
				err := abortMultipartUpload(client, resp)
				fileDoneStream <- uploadFileDoneDTO{
					err:  err,
					done: false,
				}
				return
			}
			partNumber++
			completedParts = append(completedParts, completedPart)
		}
		if len(completedParts) > 0 {
			completeResponse, err := completeMultipartUpload(client, resp, completedParts)
			if err != nil {
				fileDoneStream <- uploadFileDoneDTO{
					err:  err,
					done: false,
				}
				return
			}
			fmt.Printf("Successfully uploaded file: %s\n", aws.ToString(completeResponse.Key))
		}
		fileDoneStream <- uploadFileDoneDTO{
			err:  nil,
			done: true,
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
