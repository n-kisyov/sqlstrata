package widgets

import (
	"fmt"
	"image"
	"strings"

	"gioui.org/font"
	"gioui.org/gesture"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"

	"github.com/sqlstrata/sqlstrata/internal/engine"
	"github.com/sqlstrata/sqlstrata/internal/ui/theme"
)

type nodeKind uint8

const (
	nodeCategory nodeKind = iota
	nodeObject
	nodeColumn
)

type treeNode struct {
	id       string
	kind     nodeKind
	depth    int
	label    string
	sub      string
	badge    string
	obj      *engine.Object
	expand   bool
	hasChild bool
}

// TreeView renders the database schema tree using a virtualized list of
// flattened visible nodes.
type TreeView struct {
	schema   *engine.Schema
	filter   widget.Editor
	list     layout.List
	selected string

	collapsed map[string]bool
	expanded  map[string]bool // object names with expanded column subtrees
	nodes     []treeNode
	clicks    []gesture.Click
	dirty     bool
	lastFilt  string

	// OnSelect is invoked when the user selects a table or view to browse.
	OnSelect func(obj *engine.Object)
}

// NewTreeView constructs a schema TreeView.
func NewTreeView() *TreeView {
	t := &TreeView{
		list:      layout.List{Axis: layout.Vertical},
		collapsed: map[string]bool{},
		expanded:  map[string]bool{},
		dirty:     true,
	}
	t.filter.SingleLine = true
	return t
}

// SetSchema replaces the displayed schema.
func (t *TreeView) SetSchema(s *engine.Schema) {
	t.schema = s
	t.dirty = true
}

// Select sets the active object name (for tables/views) or node id.
func (t *TreeView) Select(name string) {
	t.selected = name
}

// SelectObject selects a specific schema object by type and name.
func (t *TreeView) SelectObject(obj *engine.Object) {
	if obj == nil {
		t.selected = ""
		return
	}
	t.selected = "obj:" + string(obj.Type) + ":" + obj.Name
}

func (t *TreeView) rebuild() {
	t.nodes = t.nodes[:0]
	if t.schema == nil {
		t.dirty = false
		return
	}
	q := strings.ToLower(strings.TrimSpace(t.filter.Text()))

	groups := []struct {
		key   string
		title string
		typ   engine.ObjectType
	}{
		{"cat:table", "Tables", engine.TypeTable},
		{"cat:view", "Views", engine.TypeView},
		{"cat:index", "Indexes", engine.TypeIndex},
		{"cat:trigger", "Triggers", engine.TypeTrigger},
	}

	for _, g := range groups {
		objs := t.schema.ByType(g.typ)
		if q != "" {
			var filtered []*engine.Object
			for _, o := range objs {
				if strings.Contains(strings.ToLower(o.Name), q) {
					filtered = append(filtered, o)
				}
			}
			objs = filtered
		}
		if len(objs) == 0 {
			continue
		}
		catCollapsed := t.collapsed[g.key] && q == ""
		t.nodes = append(t.nodes, treeNode{
			id:       g.key,
			kind:     nodeCategory,
			depth:    0,
			label:    g.title,
			badge:    fmt.Sprintf("%d", len(objs)),
			expand:   !catCollapsed,
			hasChild: true,
		})
		if catCollapsed {
			continue
		}
		for _, o := range objs {
			id := "obj:" + string(o.Type) + ":" + o.Name
			exp := t.expanded[id]
			sub := ""
			if o.WithoutRowid {
				sub = "WITHOUT ROWID"
			} else if o.Type == engine.TypeIndex || o.Type == engine.TypeTrigger {
				sub = o.Table
			}
			t.nodes = append(t.nodes, treeNode{
				id:       id,
				kind:     nodeObject,
				depth:    1,
				label:    o.Name,
				sub:      sub,
				obj:      o,
				expand:   exp,
				hasChild: len(o.Columns) > 0,
			})
			if exp {
				for _, c := range o.Columns {
					tag := c.Type
					if c.PK > 0 {
						tag = "PK · " + tag
					} else if c.NotNull {
						tag = "NN · " + tag
					}
					t.nodes = append(t.nodes, treeNode{
						id:    id + ":col:" + c.Name,
						kind:  nodeColumn,
						depth: 2,
						label: c.Name,
						sub:   strings.TrimSuffix(tag, " · "),
					})
				}
			}
		}
	}
	if len(t.clicks) < len(t.nodes) {
		t.clicks = make([]gesture.Click, len(t.nodes))
	}
	t.dirty = false
}

// Layout renders the filter bar and virtualized schema tree.
func (t *TreeView) Layout(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	theme.Fill(gtx, th.Chrome)

	for {
		if _, ok := t.filter.Update(gtx); !ok {
			break
		}
	}
	if cur := t.filter.Text(); cur != t.lastFilt {
		t.lastFilt = cur
		t.dirty = true
	}
	if t.dirty {
		t.rebuild()
	}

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return t.layoutFilter(gtx, th)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			if len(t.nodes) == 0 {
				return layout.Inset{Top: 16, Left: 12, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					msg := "No database open"
					if t.schema != nil {
						msg = "No matching objects"
					}
					return th.Label(gtx, th.Small, th.TextSubtle, msg)
				})
			}
			return t.list.Layout(gtx, len(t.nodes), func(gtx layout.Context, i int) layout.Dimensions {
				return t.layoutRow(gtx, th, i)
			})
		}),
	)
}

func (t *TreeView) layoutFilter(gtx layout.Context, th *theme.Theme) layout.Dimensions {
	h := gtx.Dp(36)
	padX := gtx.Dp(8)
	padY := gtx.Dp(6)
	w := gtx.Constraints.Max.X

	box := clip.RRect{
		Rect: image.Rect(padX, padY, w-padX, h-padY),
		NE:   gtx.Dp(4), NW: gtx.Dp(4), SE: gtx.Dp(4), SW: gtx.Dp(4),
	}
	theme.FillRRect(gtx, box, th.Surface)

	// Subtle 1px border around filter box.
	border := clip.Stroke{Path: box.Path(gtx.Ops), Width: float32(max(1, gtx.Dp(1)))}.Op()
	paint.FillShape(gtx.Ops, th.Border, border)

	inner := op.Offset(image.Pt(padX+gtx.Dp(8), padY+gtx.Dp(4))).Push(gtx.Ops)
	cg := gtx
	cg.Constraints = layout.Exact(image.Pt(max(0, w-2*padX-gtx.Dp(16)), h-2*padY-gtx.Dp(8)))

	if t.filter.Len() == 0 && !gtx.Focused(&t.filter) {
		th.Label(cg, th.Small, th.TextSubtle, "Filter schema…")
	}

	m := op.Record(gtx.Ops)
	paint.ColorOp{Color: th.Text}.Add(gtx.Ops)
	textMat := m.Stop()

	ms := op.Record(gtx.Ops)
	paint.ColorOp{Color: th.Selected}.Add(gtx.Ops)
	selMat := ms.Stop()

	t.filter.Layout(cg, th.Shaper, font.Font{Typeface: th.UI}, th.Small, textMat, selMat)
	inner.Pop()

	return layout.Dimensions{Size: image.Pt(w, h)}
}

func (t *TreeView) layoutRow(gtx layout.Context, th *theme.Theme, i int) layout.Dimensions {
	n := t.nodes[i]
	click := &t.clicks[i]

	rowH := gtx.Dp(26)
	w := gtx.Constraints.Max.X

	for {
		ev, ok := click.Update(gtx.Source)
		if !ok {
			break
		}
		if ev.Kind == gesture.KindClick {
			switch n.kind {
			case nodeCategory:
				t.collapsed[n.id] = !t.collapsed[n.id]
				t.dirty = true
			case nodeObject:
				// Clicking the chevron region toggles column subtrees;
				// clicking anywhere selects the schema object (table, view, index, trigger).
				chevW := gtx.Dp(unit.Dp(8 + n.depth*12 + 16))
				if n.hasChild && (int(ev.Position.X) <= chevW || ev.NumClicks >= 2) {
					t.expanded[n.id] = !t.expanded[n.id]
					t.dirty = true
				}
				if n.obj != nil {
					t.selected = n.id
					if t.OnSelect != nil {
						t.OnSelect(n.obj)
					}
				}
			}
		}
	}

	isSel := false
	if n.kind == nodeObject && n.obj != nil {
		if strings.HasPrefix(t.selected, "obj:") {
			isSel = n.id == t.selected
		} else if t.selected != "" {
			isSel = strings.EqualFold(n.obj.Name, t.selected)
		}
	}
	if isSel {
		theme.FillRect(gtx, clip.Rect{Min: image.Pt(4, 1), Max: image.Pt(w-4, rowH-1)}, th.Selected)
		// Left accent indicator bar.
		theme.FillRect(gtx, clip.Rect{Min: image.Pt(2, 5), Max: image.Pt(5, rowH-5)}, th.Accent)
	} else if click.Hovered() {
		theme.FillRect(gtx, clip.Rect{Min: image.Pt(4, 1), Max: image.Pt(w-4, rowH-1)}, th.Hover)
	}

	indent := gtx.Dp(unit.Dp(8 + n.depth*12))
	yText := gtx.Dp(5)

	if n.hasChild {
		glyph := "▸"
		if n.expand {
			glyph = "▾"
		}
		off := op.Offset(image.Pt(indent, yText)).Push(gtx.Ops)
		th.Label(gtx, th.Small, th.TextMuted, glyph)
		off.Pop()
	}

	xLabel := indent + gtx.Dp(14)
	labelCol := th.Text
	fnt := font.Font{Typeface: th.UI}
	switch n.kind {
	case nodeCategory:
		fnt.Weight = font.SemiBold
		labelCol = th.TextMuted
	case nodeColumn:
		labelCol = th.TextMuted
	}
	if isSel {
		labelCol = th.SelectedText
		fnt.Weight = font.SemiBold
	}

	off := op.Offset(image.Pt(xLabel, yText)).Push(gtx.Ops)
	cg := gtx
	cg.Constraints.Max.X = max(0, w-xLabel-gtx.Dp(48))
	th.LabelFont(cg, fnt, th.UISize, labelCol, text.Start, n.label)
	off.Pop()

	rightTxt := n.sub
	if n.badge != "" {
		rightTxt = n.badge
	}
	if rightTxt != "" {
		off := op.Offset(image.Pt(0, gtx.Dp(6))).Push(gtx.Ops)
		cg := gtx
		cg.Constraints.Min.X = w - gtx.Dp(10)
		cg.Constraints.Max.X = w - gtx.Dp(10)
		th.LabelFont(cg, font.Font{Typeface: th.Mono}, th.Small, th.TextSubtle, text.End, rightTxt)
		off.Pop()
	}

	area := clip.Rect{Max: image.Pt(w, rowH)}.Push(gtx.Ops)
	pointer.CursorPointer.Add(gtx.Ops)
	click.Add(gtx.Ops)
	area.Pop()

	return layout.Dimensions{Size: image.Pt(w, rowH)}
}
