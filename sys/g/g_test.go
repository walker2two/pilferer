package g

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestGetG(t *testing.T) {
	g := GetG()

	if g == nil {
		t.Fatalf("failed to get G, g is nil")
	}

	t.Log("*g =", g)
	t.Log("g =", fmt.Sprintf("%#v", (*G)(g)))

	t.Log("goroutine id = ", CurG().GoID)
}

func TestGetM(t *testing.T) {
	m := GetM()

	if m == nil {
		t.Fatalf("failed to get M, m is nil")
	}

	t.Log("*m =", m)
	t.Log("m =", fmt.Sprintf("%#v", (*M)(m)))

	mFromG := unsafe.Pointer((*G)(GetG()).m)
	if m != mFromG {
		t.Error("M and M we get from G are not the same", m, mFromG)
	}
}
