package server

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A yield must stop the whole worker (FM-R-107). `daisugi tree run` starts
// sprig, and sprig starts the model's tools, each in a process group of its
// own, so a signal to the group of tree run alone leaves sprig at work. The
// freeze walks the tree of processes below the worker, collects every group
// it finds, and stops each one.

// procRow is one process: its parent, its group and its state letter.
type procRow struct {
	ppid, pgrp int
	state      byte
}

// readProcs reads /proc once. A process that ends while it is read is left out.
func readProcs() map[int]procRow {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := map[int]procRow{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 >= len(s) {
			continue
		}
		f := strings.Fields(s[i+2:])
		if len(f) < 3 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		pgrp, _ := strconv.Atoi(f[2])
		out[pid] = procRow{ppid: ppid, pgrp: pgrp, state: f[0][0]}
	}
	return out
}

// treeOf is root and every process below it, with the groups they are in.
func treeOf(root int, procs map[int]procRow) (pids []int, groups map[int]bool) {
	groups = map[int]bool{}
	if _, ok := procs[root]; !ok {
		return nil, groups
	}
	kids := map[int][]int{}
	for pid, r := range procs {
		kids[r.ppid] = append(kids[r.ppid], pid)
	}
	todo := []int{root}
	seen := map[int]bool{root: true}
	for len(todo) > 0 {
		p := todo[0]
		todo = todo[1:]
		pids = append(pids, p)
		groups[procs[p].pgrp] = true
		for _, k := range kids[p] {
			if !seen[k] {
				seen[k] = true
				todo = append(todo, k)
			}
		}
	}
	return pids, groups
}

// safeGroup is a group a worker's freeze may signal: never 0 or 1, and never
// coppice's own group.
func safeGroup(g int) bool {
	return g > 1 && g != syscall.Getpgrp()
}

// freezeTree stops every process group of the worker rooted at root and
// returns the groups. It reports false, with everything resumed, when a
// process of the tree did not stop: the caller must not count the room free.
func freezeTree(root int) ([]int, bool) {
	if root <= 1 {
		return nil, false
	}
	stopped := map[int]bool{}
	var order []int
	deadline := time.Now().Add(2 * time.Second)
	for {
		// A stop is a signal, and a stopped process may have forked just
		// before it: look again until the kernel shows the whole tree stopped.
		_, groups := treeOf(root, readProcs())
		for g := range groups {
			if stopped[g] || !safeGroup(g) {
				continue
			}
			if err := syscall.Kill(-g, syscall.SIGSTOP); err == nil {
				stopped[g] = true
				order = append(order, g)
			}
		}
		if len(order) > 0 && allStopped(root) {
			return order, true
		}
		if time.Now().After(deadline) {
			thawGroups(order)
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// allStopped reports whether every live process of the tree under root is
// stopped. A zombie counts as stopped; a process that is gone is no process.
func allStopped(root int) bool {
	procs := readProcs()
	pids, _ := treeOf(root, procs)
	for _, p := range pids {
		if st := procs[p].state; st != 'T' && st != 't' && st != 'Z' && st != 'X' {
			return false
		}
	}
	return len(pids) > 0
}

// thawTree resumes the groups a freeze recorded, and any group the tree has
// now.
func thawTree(root int, groups []int) {
	seen := map[int]bool{}
	all := append([]int{}, groups...)
	if root > 1 {
		_, now := treeOf(root, readProcs())
		for g := range now {
			all = append(all, g)
		}
	}
	for _, g := range all {
		if seen[g] || !safeGroup(g) {
			continue
		}
		seen[g] = true
		_ = syscall.Kill(-g, syscall.SIGCONT)
	}
}

func thawGroups(groups []int) { thawTree(0, groups) }
