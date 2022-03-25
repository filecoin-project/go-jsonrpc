package json

import (
	sysJson "encoding/json"
	"io"

	jsoNiter "github.com/json-iterator/go"
)

type RawMessage = sysJson.RawMessage

var instance = jsoNiter.ConfigCompatibleWithStandardLibrary

func Marshal(obj interface{}) ([]byte, error) {
	return instance.Marshal(obj)
}

func MarshalIndent(obj interface{}, prefix, indent string) ([]byte, error) {
	return instance.MarshalIndent(obj, prefix, indent)
}

func MarshalToString(obj interface{}) (string, error) {
	return instance.MarshalToString(obj)
}

func Unmarshal(data []byte, v interface{}) error {
	return instance.Unmarshal(data, v)
}

func UnmarshalString(data string, v interface{}) error {
	return instance.Unmarshal([]byte(data), v)
}

func ToMap(v interface{}) (map[string]interface{}, error) {
	if v == nil {
		return nil, nil
	}
	bytes, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	result := make(map[string]interface{})
	err = Unmarshal(bytes, &result)
	return result, err
}

func NewDecoder(reader io.Reader) *jsoNiter.Decoder {
	return jsoNiter.NewDecoder(reader)
}

func NewEncoder(writer io.Writer) *jsoNiter.Encoder {
	return jsoNiter.NewEncoder(writer)
}
