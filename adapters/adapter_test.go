package adapters

import (
	"testing"

	runtimeevents "github.com/hollis-labs/go-runtime-events/runtimeevents"
)

type fakeAdapter struct{}

func (fakeAdapter) Name() string { return "fake" }
func (fakeAdapter) Describe() Descriptor {
	return Descriptor{
		Provider: "fake",
		Runtime:  "pty",
		Channels: []runtimeevents.SourceChannel{runtimeevents.ChannelPTY},
	}
}
func (fakeAdapter) Resolve(rc ResolveContext) (Spec, error) {
	return Spec{Binary: "/bin/echo", Args: []string{"hi"}, Cwd: rc.Cwd}, nil
}

func TestAdapterContract(t *testing.T) {
	var a Adapter = fakeAdapter{}
	if a.Name() != "fake" {
		t.Errorf("Name = %q, want fake", a.Name())
	}
	desc := a.Describe()
	if desc.Provider != "fake" || desc.Runtime != "pty" {
		t.Errorf("Describe = %+v", desc)
	}
	spec, err := a.Resolve(ResolveContext{Cwd: "/tmp/x"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if spec.Binary != "/bin/echo" || spec.Cwd != "/tmp/x" {
		t.Errorf("Spec = %+v", spec)
	}
}
