package template

import (
	"fmt"
	"testing"
)

// TestPySetOrder checks set operations come out in CPython's set order.
func TestPySetOrder(t *testing.T) {
	ops := []int{setUnion, setIntersect, setDifference, setSymmetricDifference}
	for i, c := range pySetVectors {
		for j, op := range ops {
			got, err := pySetOp(op, c[0], c[1], [2]string{"list", "list"})
			if err != nil {
				t.Fatalf("case %d op %d: %v", i, op, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c[2+j]) {
				t.Errorf("case %d op %d: %v op %v\n got %v\nwant %v", i, op, c[0], c[1], got, c[2+j])
			}
		}
	}
}
