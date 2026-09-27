package failures

import (
	"errors"
	"fmt"
	"log"
)

var ErrMissing = errors.New("missing")

type Problem struct{}

func (*Problem) Error() string { return "problem" }

func Load() error { return ErrMissing }

func Run() error {
	err := Load()
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	return nil
}

func Handle() {
	err := Load()
	if errors.Is(err, ErrMissing) {
		return
	}
	log.Print(err)
}

func Guard(value any) {
	defer func() { _ = recover() }()
	panic(value)
}
