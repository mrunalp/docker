package sysinit

import (
	"flag"
	"fmt"
	"github.com/dotcloud/docker/daemon/execdriver"
	_ "github.com/dotcloud/docker/daemon/execdriver/lxc"
	_ "github.com/dotcloud/docker/daemon/execdriver/native"
	"log"
	"os"
)

func executeProgram(args *execdriver.InitArgs) error {
	dockerInitFct, err := execdriver.GetInitFunc(args.Driver)
	if err != nil {
		panic(err)
	}
	return dockerInitFct(args)
}

// Sys Init code
// This code is run INSIDE the container and is responsible for setting
// up the environment before running the actual process
func SysInit() {
	if len(os.Args) <= 1 {
		fmt.Println("You should not invoke dockerinit manually")
		os.Exit(1)
	}

	var (
		// Get cmdline arguments
		user       = flag.String("u", "", "username or uid")
		gateway    = flag.String("g", "", "gateway address")
		ip         = flag.String("i", "", "ip address")
		workDir    = flag.String("w", "", "workdir")
		privileged = flag.Bool("privileged", false, "privileged mode")
		mtu        = flag.Int("mtu", 1500, "interface mtu")
		driver     = flag.String("driver", "", "exec driver")
		pipe       = flag.Int("pipe", 0, "sync pipe fd")
		console    = flag.String("console", "", "console (pty slave) path")
		root       = flag.String("root", ".", "root path for configuration files")
	)
	flag.Parse()

	args := &execdriver.InitArgs{
		User:       *user,
		Gateway:    *gateway,
		Ip:         *ip,
		WorkDir:    *workDir,
		Privileged: *privileged,
		Args:       flag.Args(),
		Mtu:        *mtu,
		Driver:     *driver,
		Console:    *console,
		Pipe:       *pipe,
		Root:       *root,
	}

	logFile, err := os.OpenFile("/tmp/sysinit.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalln("Failed to open log file", err)
	}

	logF := log.New(logFile, "SYSINIT: ", log.Ldate|log.Ltime|log.Lshortfile)

	logF.Println("Executing ", args)

	if err := executeProgram(args); err != nil {
		logF.Println(err)
		log.Fatal(err)
	}
}
