package medios

import "testing"

// The README and docs/writing-a-framework.md tell a framework to carve its task
// resources out of the offered ones. This compiles and runs that snippet, so a
// signature change breaks the build rather than leaving the docs wrong.
func TestDocSnippet_CarveFromOffer(t *testing.T) {
	offer := Offer{Resources: []Resource{
		{Name: "cpus", Type: "SCALAR", Scalar: &Scalar{Value: 8}},
		{Name: "mem", Type: "SCALAR", Scalar: &Scalar{Value: 8192}},
	}}

	offeredCPU, ok := FindResource(offer, "cpus")
	if !ok {
		t.Fatal("offer has no cpus")
	}
	cpus, ok := offeredCPU.Carve(0.1)
	if !ok {
		t.Fatal("cannot carve 0.1 cpus")
	}
	offeredMem, _ := FindResource(offer, "mem")
	mem, _ := offeredMem.Carve(32)

	submitter := "operator:alice"
	task, err := NewTaskInfo(submitter, "work-0",
		ID{Value: "work-0"}, offer.NodeID,
		[]Resource{cpus, mem},
		CommandInfo{Shell: true, Value: "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Resources) != 2 || task.Resources[0].Scalar.Value != 0.1 {
		t.Fatalf("carved resources = %+v", task.Resources)
	}
}
