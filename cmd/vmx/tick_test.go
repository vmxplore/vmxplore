package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Only one console-tick chain may live: a tick from a chain that newTick
// retired is not re-armed, and the live chain re-arms with its own
// generation. Opening panes used to stack chains (audit, 2026-10-01).
func TestOnlyTheLiveTickChainRearms(t *testing.T) {
	m := newModel(0, 0, 120)
	m.con = &console{kind: conSerial} // an open pane keeps the tick alive
	_ = m.newTick()
	_ = m.newTick() // a second start retires the first chain
	if m.tickGen != 2 {
		t.Fatalf("tickGen = %d, want 2", m.tickGen)
	}

	_, cmd := m.Update(conTickMsg{gen: 1})
	if cmd != nil {
		t.Fatal("a tick from a retired chain re-armed; chains would multiply")
	}

	_, cmd = m.Update(conTickMsg{gen: 2})
	if cmd == nil {
		t.Fatal("the live chain did not re-arm while a pane is open")
	}
	var gens []int
	var walk func(tea.Msg)
	walk = func(msg tea.Msg) {
		switch v := msg.(type) {
		case conTickMsg:
			gens = append(gens, v.gen)
		case tea.BatchMsg:
			for _, c := range v {
				if c != nil {
					walk(c())
				}
			}
		}
	}
	walk(cmd())
	if len(gens) != 1 || gens[0] != 2 {
		t.Fatalf("re-armed ticks carry generations %v, want exactly [2]", gens)
	}
}
