package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jalberto/qilla/internal/config"
	"github.com/jalberto/qilla/internal/doctor"
	"github.com/jalberto/qilla/internal/manifest"
)

// cmdDoctor: qilla doctor [--json] | qilla doctor fix
func cmdDoctor(args []string) error {
	if len(args) > 0 && args[0] == "fix" {
		return doctorFix(args[1:])
	}
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable report")
	healthOnly := fs.Bool("health", false, "only the [health] watch (units, freshness, jobs): one line per problem, exit 1 if any")
	count := fs.Bool("count", false, "with --health: print the number of problems and exit 0")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := config.DefaultPath()
	cfg, err := config.Load(path)
	if *healthOnly {
		if err != nil {
			return err
		}
		return doctorHealth(doctor.Health(cfg, doctor.Default(path)), *count, *asJSON)
	}
	env := doctor.Default(path)
	if cfg != nil {
		menv := manifestEnv(cfg)
		env.RoutineCheck = func(name string) ([]manifest.Result, error) {
			rs, _, err := checkRoutine(cfg, name, menv)
			return rs, err
		}
	}
	cs := doctor.Run(cfg, err, env)
	if *asJSON {
		os.Stdout.Write(doctor.JSON(cs))
		fmt.Println()
	} else {
		fmt.Print(doctor.Format(cs))
	}
	if doctor.HardFailure(cs) {
		os.Exit(1)
	}
	return nil
}

// doctorHealth prints the watch alone: nothing when
// green, one line per problem otherwise, exit 1.
func doctorHealth(cs []doctor.Check, count, asJSON bool) error {
	var bad []doctor.Check
	for _, c := range cs {
		if !c.OK {
			bad = append(bad, c)
		}
	}
	switch {
	case count:
		fmt.Println(len(bad))
		return nil
	case asJSON:
		os.Stdout.Write(doctor.JSON(cs))
		fmt.Println()
	default:
		for _, c := range bad {
			fmt.Printf("%s: %s\n", c.Name, c.Info)
		}
	}
	if len(bad) > 0 {
		os.Exit(1)
	}
	return nil
}

// doctorFix: qilla doctor fix — restart every failed watched unit
// ([health].watched_services) and report what recovered.
func doctorFix(args []string) error {
	fs := flag.NewFlagSet("doctor fix", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := config.DefaultPath()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	lines, ok := doctor.Fix(cfg, doctor.Default(path))
	for _, l := range lines {
		fmt.Println(l)
	}
	if !ok {
		os.Exit(1)
	}
	return nil
}
