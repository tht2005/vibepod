package main

import (
	"fmt"
	"os"
	"strings"
)

// Two names, one binary.
//
//	vibepod   the cockpit, and the pod's lifecycle: up, down, doctor
//	vp        the verb surface you and the agent both type all day
//
// v1's `vpctl` is retired. It was one name doing both jobs, which made the
// common case — "run this one command over there" — as long to type as the rare
// one.

const usageVibepod = `vibepod - run your agent here, run its commands where the code lives

  vibepod                      open the cockpit for this project's pod
  vibepod [pod]                open the cockpit for a named pod
  vibepod up [name]            create the pod, detached, no cockpit
  vibepod down [pod]           stop it, unmount, disconnect
  vibepod doctor               check this machine can host a pod

Everything else is vp. Run vp with no arguments for the list.

A pod takes its name and its mounts from ./vibepod.yaml unless you name one.
`

const usageVp = `vp - the machine is chosen, not guessed

  where am I
    vp backend                 which machine is this session on
    vp hosts                   the machines this pod knows, mounted or not
    vp where [@machine]        what this directory is called there, or the reverse
    vp tree                    the mounts, and what is running on which machine
    vp log [-f]                every command, and where it ran
    vp ps                      pods, sessions, backends

  move
    vp use <machine|pod>       move this session's backend
    vp @<machine> cmd ...      run one command there; the session does not move
    vp cd @<machine>           the directory that machine owns

  change the pod while it runs
    vp mount <host>:<path> [at]  connect and mount, without losing a session
    vp unmount <path|@machine>   unmount and disconnect
    vp node [add|drop <host>]     the machines running a pod of their own
    vp forward [host:port[:local]] a remote port on localhost here; no args lists them
    vp save                      write the live state back to vibepod.yaml

  terminals
    vp shell [pod]             another terminal on a running pod
    vp attach [pod] [session]  return to a session you detached from
    vp run [pod] -- cmd ...    run one command in a pod, creating it if needed

  vp brief                     the instructions the agent in this pod was given
`

func runCli(args []string, asVibepod bool) int {
	if len(args) == 0 {
		if asVibepod {
			return cmdCockpit(nil)
		}
		fmt.Print(usageVp)
		return 0
	}
	// `vp @gpu03 rocm-smi` — the sigil is the verb. It reads as an address
	// because that is what it is, and it keeps the common case to three words.
	if strings.HasPrefix(args[0], "@") {
		return cmdDispatch(strings.TrimPrefix(args[0], "@"), args[1:])
	}
	var err error
	switch args[0] {
	case "up":
		err = cmdUp(args[1:])
	case "down":
		err = cmdDown(args[1:])
	case "doctor":
		err = cmdDoctor()
	case "run":
		return cmdRun(args[1:])
	case "shell":
		return cmdShell(args[1:])
	case "attach":
		return cmdAttach(args[1:])
	case "tool":
		// Invoked by a wrapper in /vp/bin, not usually by a person.
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "vp tool: which tool?")
			return 2
		}
		return cmdTool(args[1], args[2:])
	case "use":
		err = cmdUse(args[1:])
	case "backend":
		err = cmdBackend()
	case "hosts", "machines":
		err = cmdHosts(args[1:])
	case "where":
		err = cmdWhere(args[1:])
	case "cd":
		err = cmdCd(args[1:])
	case "forward", "ports":
		err = cmdForward(args[1:])
	case "node", "nodes":
		err = cmdNode(args[1:])
	case "mount":
		err = cmdMount(args[1:])
	case "unmount", "umount":
		err = cmdUnmount(args[1:])
	case "save":
		err = cmdSave(args[1:])
	case "brief":
		err = cmdBrief(args[1:])
	case "log":
		err = cmdLog(args[1:])
	case "tree":
		err = cmdTree(args[1:])
	case "ps":
		err = cmdPs()
	case "cockpit", "tui":
		return cmdCockpit(args[1:])
	case "-h", "--help", "help":
		if asVibepod {
			fmt.Print(usageVibepod)
		} else {
			fmt.Print(usageVp)
		}
		return 0
	default:
		// A bare pod name opens its cockpit, which is what `vibepod work` should
		// obviously do and what `vp work` should obviously not.
		if asVibepod && !strings.HasPrefix(args[0], "-") {
			return cmdCockpit(args)
		}
		fmt.Fprintf(os.Stderr, "vp: unknown command %q\n\n%s", args[0], usageVp)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vibepod:", err)
		return 1
	}
	return 0
}
