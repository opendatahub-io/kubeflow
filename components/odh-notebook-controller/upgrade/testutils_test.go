package suite_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

// TestEventuallyAndConsistently showcases Gomega's two assertion helpers.
// Eventually and Consistently, and how they can be used together to check for
// what we may fairly call EventuallyConsistent state. That's to say when things stop
// changing eventually but not necessarily right away.
func TestEventuallyAndConsistently(t *testing.T) {
	// we need something like this to use Gomega in a regular go tests
	RegisterTestingT(t)
	Expect(2).To(Equal(2), "just to check the world is still sane")

	// `i` is increasing on every Eventually check, so eventually it will be equal to 3
	i := 0
	Eventually(func(g Gomega) {
		i++
		g.Expect(i).To(Equal(3))
	}).Should(Succeed())

	// same as before, but we check that `j` also remains at 3 (using default timeouts)
	j := 0
	Eventually(func(g Gomega) {
		j++
		g.Consistently(func(gg Gomega) {
			gg.Expect(j).To(Equal(3))
		}).Should(Succeed())
	}).Should(Succeed())

	// the above example, but this time we'll rewrite it using the
	// EventuallyConsistently helper defined below

	// we need a struct (plain variable would do too) to hold the state
	type s struct {
		I int
	}

	// here's the main demo, so kids, gather round
	EventuallyConsistently(ECCheck[s]{
		// initialize state to some initial value
		State: s{I: 0},
		// in the Eventually part we fetch current value
		Fetch: func(g Gomega, s *s) {
			s.I++
		},
		// in the Consistently check we compare expectation
		// with the current value
		Check: func(g Gomega, s *s) {
			g.Expect(s.I).To(Equal(3))
		},
		// and we ewt very low ConsistentDuration as not to waste time
		ConsistentDuration: time.Duration(1),
	})
}

type ECCheck[T any] struct {
	// State holds the values at the beginning of the Consistently check
	State T
	// Fetch function runs in the Eventually block and initializes State to current value
	Fetch func(g Gomega, s *T)
	// Check function runs in the Consistently block and compares State with (freshly fetched) current values
	Check func(g Gomega, s *T)

	// EventualTimeout is the timeout for the enclosing Eventually block, how log we'll keep waiting for consistency
	EventualTimeout time.Duration
	// ConsistentInterval is the period to run the Consistently check with
	ConsistentInterval time.Duration
	// ConsistentDuration is the duration for which we require consistent results for the check to pass
	ConsistentDuration time.Duration
}

// EventuallyConsistently runs the s.Check function to fetch current state and then uses
// the Consistently assertion to ensure that the state does not change for ConsistentDuration.
// The whole thing runs in an Eventually assertion, so things don't need to be consistent right away,
// but if they become consistent within EventualTimeout it is still a pass.
func EventuallyConsistently[T any](s ECCheck[T]) {
	if s.EventualTimeout == time.Duration(0) {
		s.EventualTimeout = 10 * time.Second
	}
	if s.ConsistentInterval == time.Duration(0) {
		s.ConsistentInterval = 250 * time.Millisecond
	}
	if s.ConsistentDuration == time.Duration(0) {
		s.ConsistentDuration = 2 * time.Second
	}

	Eventually(func(g Gomega) {
		s.Fetch(g, &s.State)
		g.Consistently(func(gg Gomega) { s.Check(gg, &s.State) }).
			WithTimeout(s.ConsistentDuration).WithPolling(s.ConsistentInterval).Should(Succeed())
	}).WithTimeout(s.EventualTimeout).Should(Succeed())
}
