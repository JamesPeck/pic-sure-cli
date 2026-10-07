// Package actions describes the operations the landing and the activity
// screen can launch (the dashboard runs pic-sure commands instead).
// Destructive actions must be confirmed by typing ConfirmWord and clearly
// state what is destroyed; every action carries an AbortNote so a confirmed
// abort never leaves the user guessing about state.
//
// Starting an action fails with NotImplemented until the TUI tickets run
// in-process operations. Args holds the v1 script arguments: they record the choices
// each screen collected, which the TUI tests assert, and they go away with
// that rewiring.
package actions

import (
	"fmt"
	"strings"
)

// Action is one runnable operation.
type Action struct {
	Name        string
	Ticket      string   // the v2 ticket that implements this operation
	Args        []string // the v1 script arguments; see the package doc
	Destructive bool
	ConfirmWord string
	Describe    string // shown in the confirm dialog
	AbortNote   string // shown after a confirmed mid-run abort
}

func Update() Action {
	return Action{
		Name:   "update",
		Ticket: "036",
		Describe: "Safe update: resolves release-control refs, rebuilds images,\n" +
			"runs migrations, rotates the introspection token, restarts services.\n" +
			"Data volumes are not deleted.",
		AbortNote: "update.sh is safe to re-run; it resumes from the current state.",
	}
}

func Restart(service string) Action {
	return Action{
		Name:      "restart " + service,
		Ticket:    "026",
		Args:      []string{"restart", service},
		Describe:  fmt.Sprintf("Restarts the %s service via docker compose.", service),
		AbortNote: "the service may be mid-restart; check its state in the dashboard.",
	}
}

func Preflight() Action {
	return Action{
		Name:      "preflight",
		Ticket:    "025",
		Describe:  "Non-mutating host/config validation.",
		AbortNote: "preflight is read-only; nothing changed.",
	}
}

func Migrate() Action {
	return Action{
		Name:      "migrate",
		Ticket:    "032",
		Describe:  "Runs Flyway migrations (PIC-SURE + dictionary databases), then\nrestarts psama and dictionary-api if they are running.",
		AbortNote: "re-run migrate; Flyway resumes pending migrations (pass --repair via `pic-sure migrate --repair` if it reports a failed row).",
	}
}

func SeedDB() Action {
	return Action{
		Name:      "seed-db",
		Ticket:    "033",
		Describe:  "Seeds the admin user, the visualization resource, and the\nintrospection token. Requires migrations to be applied first.\nIdempotent — safe to re-run.",
		AbortNote: "seed-db is idempotent; safe to re-run.",
	}
}

// Etl runs one parameterless etl.sh subcommand. The subcommands that need
// file arguments (load-csv, load-vcf, ...) stay CLI-only — see etl.sh -h.
func Etl(sub string) Action {
	describe := map[string]string{
		"hydrate-dictionary": "Re-hydrates the dictionary database from the currently loaded\nHPDS data.",
		"run-weights":        "Recomputes dictionary search weights using the default weights\nfile from the dictionary repo.",
		"promote-genomic":    "Promotes staged genomic data into the live HPDS data volume.",
		"public-1000genomes": "Prints the manual steps for loading the public 1000 Genomes\ngenomic dataset — downloads nothing and changes nothing.",
	}[sub]
	abort := map[string]string{
		"promote-genomic": "promotion may be partial; check the HPDS data state before re-running.",
	}[sub]
	if abort == "" {
		abort = "etl.sh " + sub + " was interrupted; it is safe to re-run."
	}
	return Action{
		Name:      "etl " + sub,
		Ticket:    etlTicket(sub),
		Args:      []string{sub},
		Describe:  describe,
		AbortNote: abort,
	}
}

// etlTicket names the v2 ticket that replaces each etl.sh subcommand.
func etlTicket(sub string) string {
	switch sub {
	case "promote-genomic":
		return "049"
	case "public-1000genomes":
		return "046"
	default: // hydrate-dictionary, run-weights
		return "044"
	}
}

// ReleaseControlApply re-resolves and applies the current release-control
// branch's refs (checkouts move; images rebuild on the next update).
func ReleaseControlApply() Action {
	return Action{
		Name:      "release-control apply",
		Ticket:    "028",
		Describe:  "Resolves the current release-control branch's refs and applies\nthem to the sibling checkouts. Run update afterwards to rebuild.",
		AbortNote: "re-run release-control; resolution and apply are idempotent.",
	}
}

// ReleaseControlDryRun resolves without applying.
func ReleaseControlDryRun() Action {
	return Action{
		Name:      "release-control dry run",
		Ticket:    "028",
		Args:      []string{"--dry-run"},
		Describe:  "Resolves the release-control refs and reports what would change\nwithout touching any checkout.",
		AbortNote: "dry run is read-only; nothing changed.",
	}
}

// ReleaseControlBranch switches the release-control branch, then resolves
// and applies it.
func ReleaseControlBranch(branch string) Action {
	return Action{
		Name:      "release-control --branch " + branch,
		Ticket:    "028",
		Args:      []string{"--branch", branch},
		Describe:  "Switches the release-control branch to '" + branch + "', resolves its\nrefs, and applies them to the sibling checkouts.",
		AbortNote: "re-run release-control; resolution and apply are idempotent.",
	}
}

// DevUp recreates one service from local source using a dev compose overlay
// (scripts/compose.sh dev up: base files + the overlay, up -d --no-deps
// --build). One-shot by design: a later plain up or update recreates the
// service from the release image.
func DevUp(overlay string) Action {
	return Action{
		Name:   "dev overlay " + overlay,
		Ticket: "052",
		Args:   []string{"dev", "up", overlay},
		Describe: "Recreates the overlay's service from LOCAL SOURCE\n" +
			"(docker-compose.dev-" + overlay + ".yml on top of the base files;\n" +
			"up -d --no-deps --build). One-shot: a later plain up or update\n" +
			"reverts the service to the release image.",
		AbortNote: "the service may be mid-recreate; re-run the overlay, or revert it to the release image.",
	}
}

// DevOff recreates a service from the release image (base compose files
// only). Accepts a service or an overlay name — the script resolves it.
func DevOff(name string) Action {
	return Action{
		Name:      "revert " + name,
		Ticket:    "052",
		Args:      []string{"dev", "off", name},
		Describe:  fmt.Sprintf("Recreates %s from the release image (base compose files only).", name),
		AbortNote: "the service may be mid-recreate; re-run the revert.",
	}
}

// Reset: destruction description matches reset.sh (backs up .env, removes
// containers, all picsure_* volumes EXCEPT the database volume, certs/,
// .data/, generated config; --yes is appended because the UI already
// confirmed). Sibling repos are untouched.
func Reset() Action { return ResetWith(false, false) }

// ResetAll: reset.sh --all — everything Reset does, PLUS the database volume,
// the PIC-SURE images, and the Maven build cache. Same typed-word gate as Reset
// (the UI distinguishes them by label and destruction text, not the word).
func ResetAll() Action { return ResetWith(true, false) }

// ResetWith builds a reset action parameterized by scope and the repo toggle,
// the way DemoData parameterizes by dataset:
//   - all=false → DB-preserving reset; all=true → reset.sh --all (full wipe)
//   - repos=true → also git-resets the sibling checkouts to their release refs
//     (reset.sh --repos): uncommitted changes are discarded, but local branches
//     and git history are KEPT. .git is never deleted (that is uninstall --repos).
//
// Reset()/ResetAll() are the zero-arg convenience wrappers for the common
// repos-off case (the dashboard and CLI use those); the combined TUI reset
// dialog calls ResetWith directly when its repo toggle is on.
func ResetWith(all, repos bool) Action {
	name := "reset"
	args := []string{}
	if all {
		name = "reset --all"
		args = append(args, "--all")
	}
	if repos {
		name += " --repos"
		args = append(args, "--repos")
	}
	args = append(args, "--yes")

	var describe string
	if all {
		describe = "FULL WIPE. Stops all containers and DELETES everything Reset does, PLUS:\n" +
			"  • the database volume (picsure-db data — ALL loaded phenotype data is lost)\n" +
			"  • every PIC-SURE image\n" +
			"  • the Maven build cache (next init rebuilds from source — slow)\n" +
			".env is backed up first; certs/, .data/, and generated config are removed too."
	} else {
		describe = "Stops all containers and DELETES:\n" +
			"  • every project volume EXCEPT the database volume (picsure-db data kept)\n" +
			"  • .env (backed up first), certs/, .data/\n" +
			"  • generated config: dictionary.env, HPDS encryption key, psama truststore"
	}
	// The repos sentence and the kept sentence are alternatives — never both
	// (saying "sibling repos are kept" while also resetting them was a bug).
	if repos {
		describe += "\nSibling repos are reset to their release refs: uncommitted changes are\n" +
			"discarded, but local branches and git history are KEPT. .env.example is kept."
	} else {
		describe += "\nSibling repos and .env.example are kept."
	}

	return Action{
		Name:        name,
		Ticket:      "056",
		Args:        args,
		Destructive: true,
		ConfirmWord: "reset",
		Describe:    describe,
		AbortNote:   "partial cleanup possible; run `pic-sure status` to see what remains.",
	}
}

// Uninstall: matches uninstall.sh --yes (compose down --volumes INCLUDING the
// database volume; .env backed up then removed; generated files removed;
// repos and images kept without extra flags).
func Uninstall() Action {
	return Action{
		Name:        "uninstall",
		Ticket:      "056",
		Args:        []string{"--yes"},
		Destructive: true,
		ConfirmWord: "uninstall",
		Describe: "Removes the Compose stack and DELETES:\n" +
			"  • all containers, networks, and volumes INCLUDING the database volume\n" +
			"  • .env (backed up first)\n" +
			"  • certs/, .data/, init.log, and all generated config\n" +
			"Cloned repos and local images are kept (use the CLI for --repos/--images).\n" +
			"NOTE: `uninstall --repos` DELETES repos/ INCLUDING local git history —\n" +
			"use `pic-sure reset --repos` to reset working trees instead of deleting.\n" +
			"Remote databases are not touched.",
		AbortNote: "partial removal possible; run `pic-sure status`, then re-run uninstall.",
	}
}

// ConfirmAccepted decides whether a completed confirm dialog authorizes the
// action: destructive actions require the typed word to match exactly (the
// yes/no flag is never bound for them); everything else uses the flag.
func ConfirmAccepted(act Action, ok bool, text string) bool {
	if act.Destructive {
		return strings.TrimSpace(text) == act.ConfirmWord
	}
	return ok
}
