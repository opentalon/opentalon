package prompts

import (
	"fmt"
	"testing"
)

func TestPrintCurrentHash(t *testing.T) {
	fmt.Println("CURRENT_HASH:", Hash())
}
