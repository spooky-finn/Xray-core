package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"github.com/getlantern/systray/example/icon"
	"github.com/xtls/xray-core/common/cmdarg"
	"github.com/xtls/xray-core/common/errors"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/platform"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/main/commands/base"
)

var cmdRun = &base.Command{
	UsageLine: "{{.Exec}} run [-c config.json] [-confdir dir]",
	Short:     "Run Xray with config, the default command",
	Long: `
Run Xray with config, the default command.

The -config=file, -c=file flags set the config files for 
Xray. Multiple assign is accepted.

The -confdir=dir flag sets a dir with multiple json config

The -format=json flag sets the format of config files. 
Default "auto".

The -test flag tells Xray to test config files only, 
without launching the server.

The -dump flag tells Xray to print the merged config.

The -sysproxy-port=port flag enables system proxy at specified port (only for macOS)

The -sysproxy-device=device flag enables system proxy at specified device (only for macOS)
	`,
}

const DefaultConfigFileLocation = "~/.xray/config.json"

func init() {
	cmdRun.Run = executeRun // break init loop
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

var (
	configFiles    cmdarg.Arg // "Config file for Xray.", the option is customed type, parse in main
	configDir      string
	dump           = cmdRun.Flag.Bool("dump", false, "Dump merged config only, without launching Xray server.")
	test           = cmdRun.Flag.Bool("test", false, "Test config file only, without launching Xray server.")
	format         = cmdRun.Flag.String("format", "auto", "Format of input file.")
	sysProxyPort   = cmdRun.Flag.String("sysproxy-port", "19800", "Enable system proxy at specified port (only for macOS)")
	sysProxyDevice = cmdRun.Flag.String("sysproxy-device", "Wi-Fi", "Enable system proxy at specified device (only for macOS)")

	/* We have to do this here because Golang's Test will also need to parse flag, before
	 * main func in this file is run.
	 */
	_ = func() bool {
		cmdRun.Flag.Var(&configFiles, "config", "Config path for Xray.")
		cmdRun.Flag.Var(&configFiles, "c", "Short alias of -config")
		cmdRun.Flag.StringVar(&configDir, "confdir", "", "A dir with multiple json config")

		return true
	}()
)

func executeRun(cmd *base.Command, args []string) {
	if runtime.GOOS == "darwin" {
		enableSysProxy(*sysProxyDevice, *sysProxyPort)
		defer disableSysProxy(*sysProxyDevice)
	}

	if *dump {
		clog.ReplaceWithSeverityLogger(clog.Severity_Warning)
		errCode := dumpConfig()
		os.Exit(errCode)
	}

	printVersion()
	server, err := startXray()
	if err != nil {
		fmt.Println("Failed to start:", err)
		// Configuration error. Exit with a special value to prevent systemd from restarting.
		os.Exit(23)
	}

	if *test {
		fmt.Println("Configuration OK.")
		os.Exit(0)
	}

	if err := server.Start(); err != nil {
		fmt.Println("Failed to start:", err)
		os.Exit(-1)
	}
	defer server.Close()

	/*
		conf.FileCache = nil
		conf.IPCache = nil
		conf.SiteCache = nil
	*/

	// Explicitly triggering GC to remove garbage from config loading.
	runtime.GC()
	debug.FreeOSMemory()

	end := make(chan struct{})
	runtime.UnlockOSThread()
	go func() error {
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)
		<-osSignals
		close(end)
		return nil
	}()
	go func() error {
		runtime.LockOSThread()
		systray.Run(onReady, onExit)
		return nil
	}()

	<-end
}

func dumpConfig() int {
	files := getConfigFilePath(false)
	if config, err := core.GetMergedConfig(files); err != nil {
		fmt.Println(err)
		time.Sleep(1 * time.Second)
		return 23
	} else {
		fmt.Print(config)
	}
	return 0
}

func fileExists(file string) bool {
	info, err := os.Stat(file)
	return err == nil && !info.IsDir()
}

func dirExists(file string) bool {
	if file == "" {
		return false
	}
	info, err := os.Stat(file)
	return err == nil && info.IsDir()
}

func getRegepxByFormat() string {
	switch strings.ToLower(*format) {
	case "json":
		return `^.+\.(json|jsonc)$`
	case "toml":
		return `^.+\.toml$`
	case "yaml", "yml":
		return `^.+\.(yaml|yml)$`
	default:
		return `^.+\.(json|jsonc|toml|yaml|yml)$`
	}
}

func readConfDir(dirPath string) {
	confs, err := os.ReadDir(dirPath)
	if err != nil {
		log.Fatalln(err)
	}
	for _, f := range confs {
		matched, err := regexp.MatchString(getRegepxByFormat(), f.Name())
		if err != nil {
			log.Fatalln(err)
		}
		if matched {
			configFiles.Set(path.Join(dirPath, f.Name()))
		}
	}
}

func getConfigFilePath(verbose bool) cmdarg.Arg {
	if dirExists(configDir) {
		if verbose {
			log.Println("Using confdir from arg:", configDir)
		}
		readConfDir(configDir)
	} else if envConfDir := platform.GetConfDirPath(); dirExists(envConfDir) {
		if verbose {
			log.Println("Using confdir from env:", envConfDir)
		}
		readConfDir(envConfDir)
	}

	if len(configFiles) > 0 {
		return configFiles
	}

	if workingDir, err := os.Getwd(); err == nil {
		configFile := filepath.Join(workingDir, "config.json")
		if fileExists(configFile) {
			if verbose {
				log.Println("Using default config: ", configFile)
			}
			return cmdarg.Arg{configFile}
		}
	}

	if configFile := platform.GetConfigurationPath(); fileExists(configFile) {
		if verbose {
			log.Println("Using config from env: ", configFile)
		}
		return cmdarg.Arg{configFile}
	}

	if verbose {
		log.Println("using default directory for search config file")
	}

	// resolve relative path to absolute path
	// Resolve the home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Println("Error getting home directory:", err)
		return cmdarg.Arg{}
	}

	var relativePath string
	// Replace the ~ with the home directory
	if len(DefaultConfigFileLocation) > 0 && DefaultConfigFileLocation[0] == '~' {
		relativePath = filepath.Join(homeDir, DefaultConfigFileLocation[1:])
	}

	// Get the absolute path
	absolutePath, err := filepath.Abs(relativePath)
	if err != nil {
		fmt.Println("Error getting absolute path:", err)
		return cmdarg.Arg{}
	}

	return cmdarg.Arg{absolutePath}
}

func getConfigFormat() string {
	f := core.GetFormatByExtension(*format)
	if f == "" {
		f = "auto"
	}
	return f
}

func startXray() (core.Server, error) {
	configFiles := getConfigFilePath(true)

	// config, err := core.LoadConfig(getConfigFormat(), configFiles[0], configFiles)

	c, err := core.LoadConfig(getConfigFormat(), configFiles)
	if err != nil {
		return nil, errors.New("failed to load config files: [", configFiles.String(), "]").Base(err)
	}

	server, err := core.New(c)
	if err != nil {
		return nil, errors.New("failed to create server").Base(err)
	}

	return server, nil
}

func background(quite *systray.MenuItem, swithSysProxyState *systray.MenuItem) {
	sysProxyState := 1

	for {
		select {
		case <-quite.ClickedCh:
			os.Exit(0)

		case <-swithSysProxyState.ClickedCh:
			{
				if sysProxyState == 1 {
					disableSysProxy(*sysProxyDevice)

					systray.SetIcon([]byte{1})
					swithSysProxyState.SetTitle("Enable")
					sysProxyState = 0
				} else {
					enableSysProxy(*sysProxyDevice, *sysProxyPort)

					systray.SetIcon(icon.Data)
					swithSysProxyState.SetTitle("Disable")
					sysProxyState = 1
				}
			}
		}

	}
}

func onReady() {
	systray.SetTitle("xray")
	systray.SetIcon(icon.Data)
	enableSysProxy := systray.AddMenuItem("Disable", "Disable/Enable system proxy")
	quite := systray.AddMenuItem("Quit", "Quit the whole app")

	go background(quite, enableSysProxy)
}

func onExit() {
	// clean up here
}

func enableSysProxy(device string, port string) {
	enableCmd := exec.Command("networksetup", "-setsocksfirewallproxy", device, "127.0.0.1", port)
	if err := enableCmd.Run(); err != nil {
		fmt.Println("Failed to set SOCKS proxy:", err)
	}

	stateCmd := exec.Command("networksetup", "-setsocksfirewallproxystate", device, "on")
	if err := stateCmd.Run(); err != nil {
		fmt.Println("Failed to enable SOCKS proxy:", err)
	}

	log.Println("Enabled system proxy for device", device, "at port", port)
}

func disableSysProxy(device string) {
	disableCmd := exec.Command("networksetup", "-setsocksfirewallproxystate", device, "off")
	if err := disableCmd.Run(); err != nil {
		fmt.Println("Failed to disable SOCKS proxy:", err)
	}
	log.Println("Disabled system proxy for device", device)
}
