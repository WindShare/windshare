package bufferbudget

import "testing"

func TestChangesBroadcastBeforeWaitAndAcrossGenerations(t *testing.T) {
	budget, err := New(1)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		first, second := budget.Changes(), budget.Changes()
		for _, changed := range []<-chan struct{}{first, second} {
			select {
			case <-changed:
				t.Fatal("new observer received an earlier change")
			default:
			}
		}
		budget.Wake()
		budget.Wake()
		for _, changed := range []<-chan struct{}{first, second} {
			select {
			case <-changed:
			default:
				t.Fatal("change before waiting was lost")
			}
		}
	}
}

func BenchmarkBudgetWake(b *testing.B) {
	for _, observe := range []bool{false, true} {
		name := "unobserved"
		if observe {
			name = "observed"
		}
		b.Run(name, func(b *testing.B) {
			budget, err := New(1)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if observe {
					budget.Changes()
				}
				budget.Wake()
			}
		})
	}
}
