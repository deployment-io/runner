package utils

import (
	"bufio"
	"bytes"
	"strings"
)

func ScanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	i := bytes.IndexByte(data, '\n')
	j := bytes.IndexByte(data, '\r')
	if i >= 0 || j >= 0 {
		// We have a full newline-terminated line.
		if i >= 0 {
			return i + 1, data[0:i], nil
		}
		//fmt.Println("data:-" + string(data[0:j]))
		return j + 1, data[0:j], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

func GetLinesFromBuffer(logBuffer *bytes.Buffer) ([]string, error) {
	var messages []string
	scanner := bufio.NewScanner(logBuffer)
	scanner.Split(ScanCRLF)
	for scanner.Scan() {
		s := scanner.Text()
		s = strings.Trim(s, " \n \r")
		messages = append(messages, s)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}
