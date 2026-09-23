package ui

import (
	"hash/fnv"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
)

// Colours are the terminal's own sixteen, never fixed RGB: they follow whatever
// theme the user already chose, light or dark, and so need no detection.
var (
	colAccent = lipgloss.Color("12")
	colDim    = lipgloss.Color("8")
	colOK     = lipgloss.Color("2")
	colFail   = lipgloss.Color("1")
	colWarn   = lipgloss.Color("3")
	// machines is the palette a backend's colour is drawn from. A machine keeps
	// its colour for as long as its name is the same, so a glance at a block's
	// bar says where it ran.
	machines = []color.Color{lipgloss.Color("6"), lipgloss.Color("5"),
		lipgloss.Color("3"), lipgloss.Color("2"), lipgloss.Color("13"),
		lipgloss.Color("14"), lipgloss.Color("11"), lipgloss.Color("10")}

	stDim    = lipgloss.NewStyle().Foreground(colDim)
	stBold   = lipgloss.NewStyle().Bold(true)
	stOK     = lipgloss.NewStyle().Foreground(colOK)
	stFail   = lipgloss.NewStyle().Foreground(colFail)
	stWarn   = lipgloss.NewStyle().Foreground(colWarn)
	stAccent = lipgloss.NewStyle().Foreground(colAccent)
)

// machineColor is a backend's colour: the accent for the pod, and a stable
// pick from the palette for every other machine.
func MachineColor(name string) color.Color {
	if name == "" || name == "pod" {
		return colAccent
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return machines[h.Sum32()%uint32(len(machines))]
}

// bar is the left edge of a block, in its machine's colour.
func bar(backend string) string {
	return lipgloss.NewStyle().Foreground(MachineColor(backend)).Render("▌") + " "
}

// tilde shortens a path under home the way a prompt would.
func tilde(path, home string) string {
	if home != "" && home != "/" && strings.HasPrefix(path, home) {
		if rest := path[len(home):]; rest == "" || rest[0] == '/' {
			return "~" + rest
		}
	}
	return path
}

// spread puts left and right on one line of width w, dropping the right side
// first when both do not fit.
func spread(left, right string, w int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+1 > w {
		if lw > w {
			return lipgloss.NewStyle().MaxWidth(w).Render(left)
		}
		return left
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}
