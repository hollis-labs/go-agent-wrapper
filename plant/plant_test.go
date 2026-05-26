package plant

import (
	"context"
	"testing"
)

func TestNoOpPlanter(t *testing.T) {
	var p Planter = NoOpPlanter{}
	r, err := p.Plant(context.Background(), "/tmp/boot", Spec{
		Files: map[string][]byte{"foo": []byte("bar")},
	})
	if err != nil {
		t.Fatalf("Plant: %v", err)
	}
	if len(r.PlantedFiles) != 0 {
		t.Errorf("NoOpPlanter reported planted files: %v", r.PlantedFiles)
	}
}
