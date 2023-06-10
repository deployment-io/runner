package utils

import (
	"fmt"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func ConvertPrimitiveAToStringSlice(a primitive.A) ([]string, error) {
	var out []string
	for _, s := range a {
		sVal, ok := s.(string)
		if !ok {
			return nil, fmt.Errorf("error convering primitive A to string slice")
		}
		out = append(out, sVal)
	}
	return out, nil
}
