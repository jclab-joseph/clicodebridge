package eventq

import "testing"

func TestOrderAndDrainOnClose(t *testing.T) {
	q := New[int]()
	for i := 0; i < 1000; i++ {
		q.Push(i) // never blocks, nobody is reading yet
	}
	q.Close()
	q.Push(-1) // ignored after Close
	n := 0
	for v := range q.Out() {
		if v != n {
			t.Fatalf("got %d want %d", v, n)
		}
		n++
	}
	if n != 1000 {
		t.Fatalf("received %d values", n)
	}
}
