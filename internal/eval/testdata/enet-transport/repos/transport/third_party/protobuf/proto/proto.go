package proto

type Message interface{}

func Marshal(value Message) ([]byte, error) { return nil, nil }
func Unmarshal(data []byte, value Message) error { return nil }
