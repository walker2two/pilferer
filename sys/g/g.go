package g

import "unsafe"

// GetG gets the current G
// Will be replaced by compiler to instructions defined in asm.s
func GetG() unsafe.Pointer

// GetM gets the current M
// Will be replaced by compiler to instructions defined in asm.s
func GetM() unsafe.Pointer

// CurG gets the current G
func CurG() *G {
	return (*G)(GetG())
}

// CurM gets the current M
func CurM() *M {
	return (*M)(GetM())
}
