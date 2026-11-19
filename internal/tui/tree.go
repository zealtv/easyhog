package tui

// row is one visible line of the flattened tree.
type row struct {
	node     *Node
	parent   string // key of the parent, empty at the top level
	depth    int
	instance *Node // the top-level ancestor, or the node itself
}

// flatten lists the visible rows: children appear only under expanded nodes,
// and hidden instances only when showHidden is set.
func flatten(nodes []Node, expanded map[string]bool, showHidden bool) []row {
	var out []row
	var walk func(ns []Node, depth int, parent string, instance *Node)
	walk = func(ns []Node, depth int, parent string, instance *Node) {
		for i := range ns {
			n := &ns[i]
			if depth == 0 && n.Hidden && !showHidden {
				continue
			}
			inst := instance
			if depth == 0 {
				inst = n
			}
			out = append(out, row{node: n, parent: parent, depth: depth, instance: inst})
			if len(n.Children) > 0 && expanded[n.Key] {
				walk(n.Children, depth+1, n.Key, inst)
			}
		}
	}
	walk(nodes, 0, "", nil)
	return out
}

func indexByKey(rows []row) map[string]int {
	idx := make(map[string]int, len(rows))
	for i, r := range rows {
		if _, dup := idx[r.node.Key]; !dup {
			idx[r.node.Key] = i
		}
	}
	return idx
}

// reselect picks the row that should be selected in rows after a snapshot,
// given the previous rows and the previous selection. The same key wins. When
// the selected node has gone, it prefers the next sibling, then the previous
// sibling, then the parent, then the nearest surviving row in the old order.
func reselect(old []row, oldSel int, rows []row) int {
	if len(rows) == 0 {
		return -1
	}
	if oldSel < 0 || oldSel >= len(old) {
		return 0
	}
	idx := indexByKey(rows)
	if i, ok := idx[old[oldSel].node.Key]; ok {
		return i
	}
	gone := old[oldSel]
	for j := oldSel + 1; j < len(old) && old[j].depth >= gone.depth; j++ {
		if old[j].depth == gone.depth && old[j].parent == gone.parent {
			if i, ok := idx[old[j].node.Key]; ok {
				return i
			}
		}
	}
	for j := oldSel - 1; j >= 0 && old[j].depth >= gone.depth; j-- {
		if old[j].depth == gone.depth && old[j].parent == gone.parent {
			if i, ok := idx[old[j].node.Key]; ok {
				return i
			}
		}
	}
	if i, ok := idx[gone.parent]; ok && gone.parent != "" {
		return i
	}
	for d := 1; d < len(old); d++ {
		for _, j := range []int{oldSel + d, oldSel - d} {
			if j >= 0 && j < len(old) {
				if i, ok := idx[old[j].node.Key]; ok {
					return i
				}
			}
		}
	}
	return 0
}
