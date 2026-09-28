package testmodel

import "testing"

func TestProduce(t *testing.T) { assertProduced(t) }

func TestDirect(t *testing.T) {
	if Produce() == "" {
		t.Fatal("empty")
	}
}

func assertProduced(t *testing.T) {
	t.Helper()
	if Produce() == "" {
		t.Fatal("empty")
	}
}
