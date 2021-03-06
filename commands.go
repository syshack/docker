package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/dotcloud/docker/archive"
	"github.com/dotcloud/docker/auth"
	"github.com/dotcloud/docker/registry"
	"github.com/dotcloud/docker/term"
	"github.com/dotcloud/docker/utils"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

var (
	GITCOMMIT string
	VERSION   string
)

var (
	ErrConnectionRefused = errors.New("Can't connect to docker daemon. Is 'docker -d' running on this host?")
)

func (cli *DockerCli) getMethod(name string) (func(...string) error, bool) {
	methodName := "Cmd" + strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
	method := reflect.ValueOf(cli).MethodByName(methodName)
	if !method.IsValid() {
		return nil, false
	}
	return method.Interface().(func(...string) error), true
}

func ParseCommands(proto, addr string, args ...string) error {
	cli := NewDockerCli(os.Stdin, os.Stdout, os.Stderr, proto, addr)

	if len(args) > 0 {
		method, exists := cli.getMethod(args[0])
		if !exists {
			fmt.Println("Error: Command not found:", args[0])
			return cli.CmdHelp(args[1:]...)
		}
		return method(args[1:]...)
	}
	return cli.CmdHelp(args...)
}

func (cli *DockerCli) CmdHelp(args ...string) error {
	if len(args) > 0 {
		method, exists := cli.getMethod(args[0])
		if !exists {
			fmt.Fprintf(cli.err, "Error: Command not found: %s\n", args[0])
		} else {
			method("--help")
			return nil
		}
	}
	help := fmt.Sprintf("Usage: docker [OPTIONS] COMMAND [arg...]\n -H=[unix://%s]: tcp://host:port to bind/connect to or unix://path/to/socket to use\n\nA self-sufficient runtime for linux containers.\n\nCommands:\n", DEFAULTUNIXSOCKET)
	for _, command := range [][]string{
		{"attach", "Attach to a running container"},
		{"build", "Build a container from a Dockerfile"},
		{"commit", "Create a new image from a container's changes"},
		{"cp", "Copy files/folders from the containers filesystem to the host path"},
		{"diff", "Inspect changes on a container's filesystem"},
		{"events", "Get real time events from the server"},
		{"export", "Stream the contents of a container as a tar archive"},
		{"history", "Show the history of an image"},
		{"images", "List images"},
		{"import", "Create a new filesystem image from the contents of a tarball"},
		{"info", "Display system-wide information"},
		{"insert", "Insert a file in an image"},
		{"inspect", "Return low-level information on a container"},
		{"kill", "Kill a running container"},
		{"login", "Register or Login to the docker registry server"},
		{"logs", "Fetch the logs of a container"},
		{"port", "Lookup the public-facing port which is NAT-ed to PRIVATE_PORT"},
		{"ps", "List containers"},
		{"pull", "Pull an image or a repository from the docker registry server"},
		{"push", "Push an image or a repository to the docker registry server"},
		{"restart", "Restart a running container"},
		{"rm", "Remove one or more containers"},
		{"rmi", "Remove one or more images"},
		{"run", "Run a command in a new container"},
		{"search", "Search for an image in the docker index"},
		{"start", "Start a stopped container"},
		{"stop", "Stop a running container"},
		{"tag", "Tag an image into a repository"},
		{"top", "Lookup the running processes of a container"},
		{"version", "Show the docker version information"},
		{"wait", "Block until a container stops, then print its exit code"},
	} {
		help += fmt.Sprintf("    %-10.10s%s\n", command[0], command[1])
	}
	fmt.Fprintf(cli.err, "%s\n", help)
	return nil
}

func (cli *DockerCli) CmdInsert(args ...string) error {
	cmd := Subcmd("insert", "IMAGE URL PATH", "Insert a file from URL in the IMAGE at PATH")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 3 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	v.Set("url", cmd.Arg(1))
	v.Set("path", cmd.Arg(2))

	if err := cli.stream("POST", "/images/"+cmd.Arg(0)+"/insert?"+v.Encode(), nil, cli.out, nil); err != nil {
		return err
	}
	return nil
}

// mkBuildContext returns an archive of an empty context with the contents
// of `dockerfile` at the path ./Dockerfile
func mkBuildContext(dockerfile string, files [][2]string) (archive.Archive, error) {
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	files = append(files, [2]string{"Dockerfile", dockerfile})
	for _, file := range files {
		name, content := file[0], file[1]
		hdr := &tar.Header{
			Name: name,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf, nil
}

func (cli *DockerCli) CmdBuild(args ...string) error {
	cmd := Subcmd("build", "[OPTIONS] PATH | URL | -", "Build a new container image from the source code at PATH")
	tag := cmd.String("t", "", "Repository name (and optionally a tag) to be applied to the resulting image in case of success")
	suppressOutput := cmd.Bool("q", false, "Suppress verbose build output")
	noCache := cmd.Bool("no-cache", false, "Do not use cache when building the image")
	rm := cmd.Bool("rm", false, "Remove intermediate containers after a successful build")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	var (
		context  archive.Archive
		isRemote bool
		err      error
	)

	if cmd.Arg(0) == "-" {
		// As a special case, 'docker build -' will build from an empty context with the
		// contents of stdin as a Dockerfile
		dockerfile, err := ioutil.ReadAll(cli.in)
		if err != nil {
			return err
		}
		context, err = mkBuildContext(string(dockerfile), nil)
	} else if utils.IsURL(cmd.Arg(0)) || utils.IsGIT(cmd.Arg(0)) {
		isRemote = true
	} else {
		if _, err := os.Stat(cmd.Arg(0)); err != nil {
			return err
		}
		context, err = archive.Tar(cmd.Arg(0), archive.Uncompressed)
	}
	var body io.Reader
	// Setup an upload progress bar
	// FIXME: ProgressReader shouldn't be this annoying to use
	if context != nil {
		sf := utils.NewStreamFormatter(false)
		body = utils.ProgressReader(ioutil.NopCloser(context), 0, cli.err, sf.FormatProgress("", "Uploading context", "%v bytes%0.0s%0.0s"), sf, true)
	}
	// Upload the build context
	v := &url.Values{}
	v.Set("t", *tag)

	if *suppressOutput {
		v.Set("q", "1")
	}
	if isRemote {
		v.Set("remote", cmd.Arg(0))
	}
	if *noCache {
		v.Set("nocache", "1")
	}
	if *rm {
		v.Set("rm", "1")
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("/v%g/build?%s", APIVERSION, v.Encode()), body)
	if err != nil {
		return err
	}
	if context != nil {
		req.Header.Set("Content-Type", "application/tar")
	}
	dial, err := net.Dial(cli.proto, cli.addr)
	if err != nil {
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	resp, err := clientconn.Do(req)
	defer clientconn.Close()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Check for errors
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("Error: %s", http.StatusText(resp.StatusCode))
		}
		return fmt.Errorf("Error: %s", body)
	}

	// Output the result
	if _, err := io.Copy(cli.out, resp.Body); err != nil {
		return err
	}

	return nil
}

// 'docker login': login / register a user to registry service.
func (cli *DockerCli) CmdLogin(args ...string) error {
	cmd := Subcmd("login", "[OPTIONS] [SERVER]", "Register or Login to a docker registry server, if no server is specified \""+auth.IndexServerAddress()+"\" is the default.")

	var username, password, email string

	cmd.StringVar(&username, "u", "", "username")
	cmd.StringVar(&password, "p", "", "password")
	cmd.StringVar(&email, "e", "", "email")
	err := cmd.Parse(args)
	if err != nil {
		return nil
	}
	serverAddress := auth.IndexServerAddress()
	if len(cmd.Args()) > 0 {
		serverAddress, err = registry.ExpandAndVerifyRegistryUrl(cmd.Arg(0))
		if err != nil {
			return err
		}
		fmt.Fprintf(cli.out, "Login against server at %s\n", serverAddress)
	}

	promptDefault := func(prompt string, configDefault string) {
		if configDefault == "" {
			fmt.Fprintf(cli.out, "%s: ", prompt)
		} else {
			fmt.Fprintf(cli.out, "%s (%s): ", prompt, configDefault)
		}
	}

	readInput := func(in io.Reader, out io.Writer) string {
		reader := bufio.NewReader(in)
		line, _, err := reader.ReadLine()
		if err != nil {
			fmt.Fprintln(out, err.Error())
			os.Exit(1)
		}
		return string(line)
	}

	cli.LoadConfigFile()
	authconfig, ok := cli.configFile.Configs[serverAddress]
	if !ok {
		authconfig = auth.AuthConfig{}
	}

	if username == "" {
		promptDefault("Username", authconfig.Username)
		username = readInput(cli.in, cli.out)
		if username == "" {
			username = authconfig.Username
		}
	}
	if username != authconfig.Username {
		if password == "" {
			oldState, _ := term.SaveState(cli.terminalFd)
			fmt.Fprintf(cli.out, "Password: ")
			term.DisableEcho(cli.terminalFd, oldState)

			password = readInput(cli.in, cli.out)
			fmt.Fprint(cli.out, "\n")

			term.RestoreTerminal(cli.terminalFd, oldState)
			if password == "" {
				return fmt.Errorf("Error : Password Required")
			}
		}

		if email == "" {
			promptDefault("Email", authconfig.Email)
			email = readInput(cli.in, cli.out)
			if email == "" {
				email = authconfig.Email
			}
		}
	} else {
		password = authconfig.Password
		email = authconfig.Email
	}
	authconfig.Username = username
	authconfig.Password = password
	authconfig.Email = email
	authconfig.ServerAddress = serverAddress
	cli.configFile.Configs[serverAddress] = authconfig

	body, statusCode, err := cli.call("POST", "/auth", cli.configFile.Configs[serverAddress])
	if statusCode == 401 {
		delete(cli.configFile.Configs, serverAddress)
		auth.SaveConfig(cli.configFile)
		return err
	}
	if err != nil {
		return err
	}

	var out2 APIAuth
	err = json.Unmarshal(body, &out2)
	if err != nil {
		cli.configFile, _ = auth.LoadConfig(os.Getenv("HOME"))
		return err
	}
	auth.SaveConfig(cli.configFile)
	if out2.Status != "" {
		fmt.Fprintf(cli.out, "%s\n", out2.Status)
	}
	return nil
}

// 'docker wait': block until a container stops
func (cli *DockerCli) CmdWait(args ...string) error {
	cmd := Subcmd("wait", "CONTAINER [CONTAINER...]", "Block until a container stops, then print its exit code.")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}
	for _, name := range cmd.Args() {
		status, err := waitForExit(cli, name)
		if err != nil {
			fmt.Fprintf(cli.err, "%s", err)
		} else {
			fmt.Fprintf(cli.out, "%d\n", status)
		}
	}
	return nil
}

// 'docker version': show version information
func (cli *DockerCli) CmdVersion(args ...string) error {
	cmd := Subcmd("version", "", "Show the docker version information.")
	if err := cmd.Parse(args); err != nil {
		return nil
	}

	if cmd.NArg() > 0 {
		cmd.Usage()
		return nil
	}
	if VERSION != "" {
		fmt.Fprintf(cli.out, "Client version: %s\n", VERSION)
	}
	fmt.Fprintf(cli.out, "Go version (client): %s\n", runtime.Version())
	if GITCOMMIT != "" {
		fmt.Fprintf(cli.out, "Git commit (client): %s\n", GITCOMMIT)
	}

	body, _, err := cli.call("GET", "/version", nil)
	if err != nil {
		return err
	}

	var out APIVersion
	err = json.Unmarshal(body, &out)
	if err != nil {
		utils.Errorf("Error unmarshal: body: %s, err: %s\n", body, err)
		return err
	}
	if out.Version != "" {
		fmt.Fprintf(cli.out, "Server version: %s\n", out.Version)
	}
	if out.GitCommit != "" {
		fmt.Fprintf(cli.out, "Git commit (server): %s\n", out.GitCommit)
	}
	if out.GoVersion != "" {
		fmt.Fprintf(cli.out, "Go version (server): %s\n", out.GoVersion)
	}

	release := utils.GetReleaseVersion()
	if release != "" {
		fmt.Fprintf(cli.out, "Last stable version: %s", release)
		if (VERSION != "" || out.Version != "") && (strings.Trim(VERSION, "-dev") != release || strings.Trim(out.Version, "-dev") != release) {
			fmt.Fprintf(cli.out, ", please update docker")
		}
		fmt.Fprintf(cli.out, "\n")
	}
	return nil
}

// 'docker info': display system-wide information.
func (cli *DockerCli) CmdInfo(args ...string) error {
	cmd := Subcmd("info", "", "Display system-wide information")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() > 0 {
		cmd.Usage()
		return nil
	}

	body, _, err := cli.call("GET", "/info", nil)
	if err != nil {
		return err
	}

	var out APIInfo
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}

	fmt.Fprintf(cli.out, "Containers: %d\n", out.Containers)
	fmt.Fprintf(cli.out, "Images: %d\n", out.Images)
	if out.Debug || os.Getenv("DEBUG") != "" {
		fmt.Fprintf(cli.out, "Debug mode (server): %v\n", out.Debug)
		fmt.Fprintf(cli.out, "Debug mode (client): %v\n", os.Getenv("DEBUG") != "")
		fmt.Fprintf(cli.out, "Fds: %d\n", out.NFd)
		fmt.Fprintf(cli.out, "Goroutines: %d\n", out.NGoroutines)
		fmt.Fprintf(cli.out, "LXC Version: %s\n", out.LXCVersion)
		fmt.Fprintf(cli.out, "EventsListeners: %d\n", out.NEventsListener)
		fmt.Fprintf(cli.out, "Kernel Version: %s\n", out.KernelVersion)
	}

	if len(out.IndexServerAddress) != 0 {
		cli.LoadConfigFile()
		u := cli.configFile.Configs[out.IndexServerAddress].Username
		if len(u) > 0 {
			fmt.Fprintf(cli.out, "Username: %v\n", u)
			fmt.Fprintf(cli.out, "Registry: %v\n", out.IndexServerAddress)
		}
	}
	if !out.MemoryLimit {
		fmt.Fprintf(cli.err, "WARNING: No memory limit support\n")
	}
	if !out.SwapLimit {
		fmt.Fprintf(cli.err, "WARNING: No swap limit support\n")
	}
	if !out.IPv4Forwarding {
		fmt.Fprintf(cli.err, "WARNING: IPv4 forwarding is disabled.\n")
	}
	return nil
}

func (cli *DockerCli) CmdStop(args ...string) error {
	cmd := Subcmd("stop", "[OPTIONS] CONTAINER [CONTAINER...]", "Stop a running container (Send SIGTERM, and then SIGKILL after grace period)")
	nSeconds := cmd.Int("t", 10, "Number of seconds to wait for the container to stop before killing it.")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	v.Set("t", strconv.Itoa(*nSeconds))

	for _, name := range cmd.Args() {
		_, _, err := cli.call("POST", "/containers/"+name+"/stop?"+v.Encode(), nil)
		if err != nil {
			fmt.Fprintf(cli.err, "%s\n", err)
		} else {
			fmt.Fprintf(cli.out, "%s\n", name)
		}
	}
	return nil
}

func (cli *DockerCli) CmdRestart(args ...string) error {
	cmd := Subcmd("restart", "[OPTIONS] CONTAINER [CONTAINER...]", "Restart a running container")
	nSeconds := cmd.Int("t", 10, "Number of seconds to try to stop for before killing the container. Once killed it will then be restarted. Default=10")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	v.Set("t", strconv.Itoa(*nSeconds))

	for _, name := range cmd.Args() {
		_, _, err := cli.call("POST", "/containers/"+name+"/restart?"+v.Encode(), nil)
		if err != nil {
			fmt.Fprintf(cli.err, "%s\n", err)
		} else {
			fmt.Fprintf(cli.out, "%s\n", name)
		}
	}
	return nil
}

func (cli *DockerCli) forwardAllSignals(cid string) chan os.Signal {
	sigc := make(chan os.Signal, 1)
	utils.CatchAll(sigc)
	go func() {
		for s := range sigc {
			if _, _, err := cli.call("POST", fmt.Sprintf("/containers/%s/kill?signal=%d", cid, s), nil); err != nil {
				utils.Debugf("Error sending signal: %s", err)
			}
		}
	}()
	return sigc
}

func (cli *DockerCli) CmdStart(args ...string) error {
	cmd := Subcmd("start", "CONTAINER [CONTAINER...]", "Restart a stopped container")
	attach := cmd.Bool("a", false, "Attach container's stdout/stderr and forward all signals to the process")
	openStdin := cmd.Bool("i", false, "Attach container's stdin")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	var cErr chan error
	if *attach || *openStdin {
		if cmd.NArg() > 1 {
			return fmt.Errorf("Impossible to start and attach multiple containers at once.")
		}

		body, _, err := cli.call("GET", "/containers/"+cmd.Arg(0)+"/json", nil)
		if err != nil {
			return err
		}

		container := &Container{}
		err = json.Unmarshal(body, container)
		if err != nil {
			return err
		}

		if !container.Config.Tty {
			sigc := cli.forwardAllSignals(cmd.Arg(0))
			defer utils.StopCatch(sigc)
		}

		if container.Config.Tty && cli.isTerminal {
			if err := cli.monitorTtySize(cmd.Arg(0)); err != nil {
				return err
			}
		}

		var in io.ReadCloser

		v := url.Values{}
		v.Set("stream", "1")
		if *openStdin && container.Config.OpenStdin {
			v.Set("stdin", "1")
			in = cli.in
		}
		v.Set("stdout", "1")
		v.Set("stderr", "1")

		cErr = utils.Go(func() error {
			return cli.hijack("POST", "/containers/"+cmd.Arg(0)+"/attach?"+v.Encode(), container.Config.Tty, in, cli.out, cli.err, nil)
		})
	}

	var encounteredError error
	for _, name := range cmd.Args() {
		_, _, err := cli.call("POST", "/containers/"+name+"/start", nil)
		if err != nil {
			if !*attach || !*openStdin {
				fmt.Fprintf(cli.err, "%s\n", err)
				encounteredError = fmt.Errorf("Error: failed to start one or more containers")
			}
		} else {
			if !*attach || !*openStdin {
				fmt.Fprintf(cli.out, "%s\n", name)
			}
		}
	}
	if encounteredError != nil {
		if *openStdin || *attach {
			cli.in.Close()
			<-cErr
		}
		return encounteredError
	}
	if *openStdin || *attach {
		return <-cErr
	}
	return nil
}

func (cli *DockerCli) CmdInspect(args ...string) error {
	cmd := Subcmd("inspect", "CONTAINER|IMAGE [CONTAINER|IMAGE...]", "Return low-level information on a container/image")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	indented := new(bytes.Buffer)
	status := 0

	for _, name := range args {
		obj, _, err := cli.call("GET", "/containers/"+name+"/json", nil)
		if err != nil {
			obj, _, err = cli.call("GET", "/images/"+name+"/json", nil)
			if err != nil {
				if strings.Contains(err.Error(), "No such") {
					fmt.Fprintf(cli.err, "Error: No such image or container: %s\n", name)
				} else {
					fmt.Fprintf(cli.err, "%s", err)
				}
				status = 1
				continue
			}
		}

		if err = json.Indent(indented, obj, "", "    "); err != nil {
			fmt.Fprintf(cli.err, "%s\n", err)
			status = 1
			continue
		}
		indented.WriteString(",")
	}

	if indented.Len() > 0 {
		// Remove trailing ','
		indented.Truncate(indented.Len() - 1)
	}
	fmt.Fprintf(cli.out, "[")
	if _, err := io.Copy(cli.out, indented); err != nil {
		return err
	}
	fmt.Fprintf(cli.out, "]")
	if status != 0 {
		return &utils.StatusError{Status: status}
	}
	return nil
}

func (cli *DockerCli) CmdTop(args ...string) error {
	cmd := Subcmd("top", "CONTAINER [ps OPTIONS]", "Lookup the running processes of a container")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() == 0 {
		cmd.Usage()
		return nil
	}
	val := url.Values{}
	if cmd.NArg() > 1 {
		val.Set("ps_args", strings.Join(cmd.Args()[1:], " "))
	}

	body, _, err := cli.call("GET", "/containers/"+cmd.Arg(0)+"/top?"+val.Encode(), nil)
	if err != nil {
		return err
	}
	procs := APITop{}
	err = json.Unmarshal(body, &procs)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cli.out, 20, 1, 3, ' ', 0)
	fmt.Fprintln(w, strings.Join(procs.Titles, "\t"))
	for _, proc := range procs.Processes {
		fmt.Fprintln(w, strings.Join(proc, "\t"))
	}
	w.Flush()
	return nil
}

func (cli *DockerCli) CmdPort(args ...string) error {
	cmd := Subcmd("port", "CONTAINER PRIVATE_PORT", "Lookup the public-facing port which is NAT-ed to PRIVATE_PORT")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 2 {
		cmd.Usage()
		return nil
	}

	port := cmd.Arg(1)
	proto := "tcp"
	parts := strings.SplitN(port, "/", 2)
	if len(parts) == 2 && len(parts[1]) != 0 {
		port = parts[0]
		proto = parts[1]
	}
	body, _, err := cli.call("GET", "/containers/"+cmd.Arg(0)+"/json", nil)
	if err != nil {
		return err
	}
	var out Container
	err = json.Unmarshal(body, &out)
	if err != nil {
		return err
	}

	if frontends, exists := out.NetworkSettings.Ports[Port(port+"/"+proto)]; exists {
		if frontends == nil {
			fmt.Fprintf(cli.out, "%s\n", port)
		} else {
			for _, frontend := range frontends {
				fmt.Fprintf(cli.out, "%s:%s\n", frontend.HostIp, frontend.HostPort)
			}
		}
	} else {
		return fmt.Errorf("Error: No private port '%s' allocated on %s", cmd.Arg(1), cmd.Arg(0))
	}
	return nil
}

// 'docker rmi IMAGE' removes all images with the name IMAGE
func (cli *DockerCli) CmdRmi(args ...string) error {
	cmd := Subcmd("rmi", "IMAGE [IMAGE...]", "Remove one or more images")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	for _, name := range cmd.Args() {
		body, _, err := cli.call("DELETE", "/images/"+name, nil)
		if err != nil {
			fmt.Fprintf(cli.err, "%s", err)
		} else {
			var outs []APIRmi
			err = json.Unmarshal(body, &outs)
			if err != nil {
				return err
			}
			for _, out := range outs {
				if out.Deleted != "" {
					fmt.Fprintf(cli.out, "Deleted: %s\n", out.Deleted)
				} else {
					fmt.Fprintf(cli.out, "Untagged: %s\n", out.Untagged)
				}
			}
		}
	}
	return nil
}

func (cli *DockerCli) CmdHistory(args ...string) error {
	cmd := Subcmd("history", "[OPTIONS] IMAGE", "Show the history of an image")
	quiet := cmd.Bool("q", false, "only show numeric IDs")
	noTrunc := cmd.Bool("notrunc", false, "Don't truncate output")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	body, _, err := cli.call("GET", "/images/"+cmd.Arg(0)+"/history", nil)
	if err != nil {
		return err
	}

	var outs []APIHistory
	err = json.Unmarshal(body, &outs)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(cli.out, 20, 1, 3, ' ', 0)
	if !*quiet {
		fmt.Fprintln(w, "IMAGE\tCREATED\tCREATED BY\tSIZE")
	}

	for _, out := range outs {
		if !*quiet {
			if *noTrunc {
				fmt.Fprintf(w, "%s\t", out.ID)
			} else {
				fmt.Fprintf(w, "%s\t", utils.TruncateID(out.ID))
			}

			fmt.Fprintf(w, "%s ago\t", utils.HumanDuration(time.Now().Sub(time.Unix(out.Created, 0))))

			if *noTrunc {
				fmt.Fprintf(w, "%s\t", out.CreatedBy)
			} else {
				fmt.Fprintf(w, "%s\t", utils.Trunc(out.CreatedBy, 45))
			}
			fmt.Fprintf(w, "%s\n", utils.HumanSize(out.Size))
		} else {
			if *noTrunc {
				fmt.Fprintln(w, out.ID)
			} else {
				fmt.Fprintln(w, utils.TruncateID(out.ID))
			}
		}
	}
	w.Flush()
	return nil
}

func (cli *DockerCli) CmdRm(args ...string) error {
	cmd := Subcmd("rm", "[OPTIONS] CONTAINER [CONTAINER...]", "Remove one or more containers")
	v := cmd.Bool("v", false, "Remove the volumes associated to the container")
	link := cmd.Bool("link", false, "Remove the specified link and not the underlying container")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}
	val := url.Values{}
	if *v {
		val.Set("v", "1")
	}
	if *link {
		val.Set("link", "1")
	}
	for _, name := range cmd.Args() {
		_, _, err := cli.call("DELETE", "/containers/"+name+"?"+val.Encode(), nil)
		if err != nil {
			fmt.Fprintf(cli.err, "%s\n", err)
		} else {
			fmt.Fprintf(cli.out, "%s\n", name)
		}
	}
	return nil
}

// 'docker kill NAME' kills a running container
func (cli *DockerCli) CmdKill(args ...string) error {
	cmd := Subcmd("kill", "CONTAINER [CONTAINER...]", "Kill a running container (send SIGKILL)")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}

	for _, name := range args {
		_, _, err := cli.call("POST", "/containers/"+name+"/kill", nil)
		if err != nil {
			fmt.Fprintf(cli.err, "%s\n", err)
		} else {
			fmt.Fprintf(cli.out, "%s\n", name)
		}
	}
	return nil
}

func (cli *DockerCli) CmdImport(args ...string) error {
	cmd := Subcmd("import", "URL|- [REPOSITORY[:TAG]]", "Create a new filesystem image from the contents of a tarball(.tar, .tar.gz, .tgz, .bzip, .tar.xz, .txz).")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() < 1 {
		cmd.Usage()
		return nil
	}
	src := cmd.Arg(0)
	repository, tag := utils.ParseRepositoryTag(cmd.Arg(1))
	v := url.Values{}
	v.Set("repo", repository)
	v.Set("tag", tag)
	v.Set("fromSrc", src)

	var in io.Reader

	if src == "-" {
		in = cli.in
	}

	return cli.stream("POST", "/images/create?"+v.Encode(), in, cli.out, nil)
}

func (cli *DockerCli) CmdPush(args ...string) error {
	cmd := Subcmd("push", "NAME", "Push an image or a repository to the registry")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	name := cmd.Arg(0)

	if name == "" {
		cmd.Usage()
		return nil
	}

	cli.LoadConfigFile()

	// Resolve the Repository name from fqn to endpoint + name
	endpoint, _, err := registry.ResolveRepositoryName(name)
	if err != nil {
		return err
	}
	// Resolve the Auth config relevant for this server
	authConfig := cli.configFile.ResolveAuthConfig(endpoint)
	// If we're not using a custom registry, we know the restrictions
	// applied to repository names and can warn the user in advance.
	// Custom repositories can have different rules, and we must also
	// allow pushing by image ID.
	if len(strings.SplitN(name, "/", 2)) == 1 {
		username := cli.configFile.Configs[auth.IndexServerAddress()].Username
		if username == "" {
			username = "<user>"
		}
		return fmt.Errorf("Impossible to push a \"root\" repository. Please rename your repository in <user>/<repo> (ex: %s/%s)", username, name)
	}

	v := url.Values{}
	push := func(authConfig auth.AuthConfig) error {
		buf, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}
		registryAuthHeader := []string{
			base64.URLEncoding.EncodeToString(buf),
		}

		return cli.stream("POST", "/images/"+name+"/push?"+v.Encode(), nil, cli.out, map[string][]string{
			"X-Registry-Auth": registryAuthHeader,
		})
	}

	if err := push(authConfig); err != nil {
		if err.Error() == registry.ErrLoginRequired.Error() {
			fmt.Fprintln(cli.out, "\nPlease login prior to push:")
			if err := cli.CmdLogin(endpoint); err != nil {
				return err
			}
			authConfig := cli.configFile.ResolveAuthConfig(endpoint)
			return push(authConfig)
		}
		return err
	}
	return nil
}

func (cli *DockerCli) CmdPull(args ...string) error {
	cmd := Subcmd("pull", "NAME", "Pull an image or a repository from the registry")
	tag := cmd.String("t", "", "Download tagged image in repository")
	if err := cmd.Parse(args); err != nil {
		return nil
	}

	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	remote, parsedTag := utils.ParseRepositoryTag(cmd.Arg(0))
	if *tag == "" {
		*tag = parsedTag
	}

	// Resolve the Repository name from fqn to endpoint + name
	endpoint, _, err := registry.ResolveRepositoryName(remote)
	if err != nil {
		return err
	}

	cli.LoadConfigFile()

	// Resolve the Auth config relevant for this server
	authConfig := cli.configFile.ResolveAuthConfig(endpoint)
	v := url.Values{}
	v.Set("fromImage", remote)
	v.Set("tag", *tag)

	pull := func(authConfig auth.AuthConfig) error {
		buf, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}
		registryAuthHeader := []string{
			base64.URLEncoding.EncodeToString(buf),
		}

		return cli.stream("POST", "/images/create?"+v.Encode(), nil, cli.out, map[string][]string{
			"X-Registry-Auth": registryAuthHeader,
		})
	}

	if err := pull(authConfig); err != nil {
		if err.Error() == registry.ErrLoginRequired.Error() {
			fmt.Fprintln(cli.out, "\nPlease login prior to push:")
			if err := cli.CmdLogin(endpoint); err != nil {
				return err
			}
			authConfig := cli.configFile.ResolveAuthConfig(endpoint)
			return pull(authConfig)
		}
		return err
	}

	return nil
}

func (cli *DockerCli) CmdImages(args ...string) error {
	cmd := Subcmd("images", "[OPTIONS] [NAME]", "List images")
	quiet := cmd.Bool("q", false, "only show numeric IDs")
	all := cmd.Bool("a", false, "show all images")
	noTrunc := cmd.Bool("notrunc", false, "Don't truncate output")
	flViz := cmd.Bool("viz", false, "output graph in graphviz format")
	flTree := cmd.Bool("tree", false, "output graph in tree format")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() > 1 {
		cmd.Usage()
		return nil
	}

	if *flViz {
		body, _, err := cli.call("GET", "/images/json?all=1", nil)
		if err != nil {
			return err
		}

		var outs []APIImages
		err = json.Unmarshal(body, &outs)
		if err != nil {
			return err
		}

		fmt.Fprintf(cli.out, "digraph docker {\n")

		for _, image := range outs {
			if image.ParentId == "" {
				fmt.Fprintf(cli.out, " base -> \"%s\" [style=invis]\n", utils.TruncateID(image.ID))
			} else {
				fmt.Fprintf(cli.out, " \"%s\" -> \"%s\"\n", utils.TruncateID(image.ParentId), utils.TruncateID(image.ID))
			}
			if image.RepoTags[0] != "<none>:<none>" {
				fmt.Fprintf(cli.out, " \"%s\" [label=\"%s\\n%s\",shape=box,fillcolor=\"paleturquoise\",style=\"filled,rounded\"];\n", utils.TruncateID(image.ID), utils.TruncateID(image.ID), strings.Join(image.RepoTags, "\\n"))
			}
		}

		fmt.Fprintf(cli.out, " base [style=invisible]\n}\n")
	} else if *flTree {
		body, _, err := cli.call("GET", "/images/json?all=1", nil)
		if err != nil {
			return err
		}

		var outs []APIImages
		err = json.Unmarshal(body, &outs)
		if err != nil {
			return err
		}

		var startImageArg = cmd.Arg(0)
		var startImage APIImages

		var roots []APIImages
		var byParent = make(map[string][]APIImages)
		for _, image := range outs {
			if image.ParentId == "" {
				roots = append(roots, image)
			} else {
				if children, exists := byParent[image.ParentId]; exists {
					byParent[image.ParentId] = append(children, image)
				} else {
					byParent[image.ParentId] = []APIImages{image}
				}
			}

			if startImageArg != "" {
				if startImageArg == image.ID || startImageArg == utils.TruncateID(image.ID) {
					startImage = image
				}

				for _, repotag := range image.RepoTags {
					if repotag == startImageArg {
						startImage = image
					}
				}
			}
		}

		if startImageArg != "" {
			WalkTree(cli, noTrunc, []APIImages{startImage}, byParent, "")
		} else {
			WalkTree(cli, noTrunc, roots, byParent, "")
		}
	} else {
		v := url.Values{}
		if cmd.NArg() == 1 {
			v.Set("filter", cmd.Arg(0))
		}
		if *all {
			v.Set("all", "1")
		}

		body, _, err := cli.call("GET", "/images/json?"+v.Encode(), nil)
		if err != nil {
			return err
		}

		var outs []APIImages
		err = json.Unmarshal(body, &outs)
		if err != nil {
			return err
		}

		w := tabwriter.NewWriter(cli.out, 20, 1, 3, ' ', 0)
		if !*quiet {
			fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE")
		}

		var repo string
		var tag string
		for _, out := range outs {
			for _, repotag := range out.RepoTags {

				components := strings.SplitN(repotag, ":", 2)
				repo = components[0]
				tag = components[1]

				if !*noTrunc {
					out.ID = utils.TruncateID(out.ID)
				}

				if !*quiet {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t", repo, tag, out.ID, utils.HumanDuration(time.Now().Sub(time.Unix(out.Created, 0))))
					if out.VirtualSize > 0 {
						fmt.Fprintf(w, "%s (virtual %s)\n", utils.HumanSize(out.Size), utils.HumanSize(out.VirtualSize))
					} else {
						fmt.Fprintf(w, "%s\n", utils.HumanSize(out.Size))
					}
				} else {
					fmt.Fprintln(w, out.ID)
				}
			}
		}

		if !*quiet {
			w.Flush()
		}
	}
	return nil
}

func WalkTree(cli *DockerCli, noTrunc *bool, images []APIImages, byParent map[string][]APIImages, prefix string) {
	if len(images) > 1 {
		length := len(images)
		for index, image := range images {
			if index+1 == length {
				PrintTreeNode(cli, noTrunc, image, prefix+"└─")
				if subimages, exists := byParent[image.ID]; exists {
					WalkTree(cli, noTrunc, subimages, byParent, prefix+"  ")
				}
			} else {
				PrintTreeNode(cli, noTrunc, image, prefix+"|─")
				if subimages, exists := byParent[image.ID]; exists {
					WalkTree(cli, noTrunc, subimages, byParent, prefix+"| ")
				}
			}
		}
	} else {
		for _, image := range images {
			PrintTreeNode(cli, noTrunc, image, prefix+"└─")
			if subimages, exists := byParent[image.ID]; exists {
				WalkTree(cli, noTrunc, subimages, byParent, prefix+"  ")
			}
		}
	}
}

func PrintTreeNode(cli *DockerCli, noTrunc *bool, image APIImages, prefix string) {
	var imageID string
	if *noTrunc {
		imageID = image.ID
	} else {
		imageID = utils.TruncateID(image.ID)
	}

	fmt.Fprintf(cli.out, "%s%s Size: %s (virtual %s)", prefix, imageID, utils.HumanSize(image.Size), utils.HumanSize(image.VirtualSize))
	if image.RepoTags[0] != "<none>:<none>" {
		fmt.Fprintf(cli.out, " Tags: %s\n", strings.Join(image.RepoTags, ","))
	} else {
		fmt.Fprint(cli.out, "\n")
	}
}

func displayablePorts(ports []APIPort) string {
	result := []string{}
	for _, port := range ports {
		if port.IP == "" {
			result = append(result, fmt.Sprintf("%d/%s", port.PublicPort, port.Type))
		} else {
			result = append(result, fmt.Sprintf("%s:%d->%d/%s", port.IP, port.PublicPort, port.PrivatePort, port.Type))
		}
	}
	sort.Strings(result)
	return strings.Join(result, ", ")
}

func (cli *DockerCli) CmdPs(args ...string) error {
	cmd := Subcmd("ps", "[OPTIONS]", "List containers")
	quiet := cmd.Bool("q", false, "Only display numeric IDs")
	size := cmd.Bool("s", false, "Display sizes")
	all := cmd.Bool("a", false, "Show all containers. Only running containers are shown by default.")
	noTrunc := cmd.Bool("notrunc", false, "Don't truncate output")
	nLatest := cmd.Bool("l", false, "Show only the latest created container, include non-running ones.")
	since := cmd.String("sinceId", "", "Show only containers created since Id, include non-running ones.")
	before := cmd.String("beforeId", "", "Show only container created before Id, include non-running ones.")
	last := cmd.Int("n", -1, "Show n last created containers, include non-running ones.")

	if err := cmd.Parse(args); err != nil {
		return nil
	}
	v := url.Values{}
	if *last == -1 && *nLatest {
		*last = 1
	}
	if *all {
		v.Set("all", "1")
	}
	if *last != -1 {
		v.Set("limit", strconv.Itoa(*last))
	}
	if *since != "" {
		v.Set("since", *since)
	}
	if *before != "" {
		v.Set("before", *before)
	}
	if *size {
		v.Set("size", "1")
	}

	body, _, err := cli.call("GET", "/containers/json?"+v.Encode(), nil)
	if err != nil {
		return err
	}

	var outs []APIContainers
	err = json.Unmarshal(body, &outs)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cli.out, 20, 1, 3, ' ', 0)
	if !*quiet {
		fmt.Fprint(w, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS\tNAMES")
		if *size {
			fmt.Fprintln(w, "\tSIZE")
		} else {
			fmt.Fprint(w, "\n")
		}
	}

	for _, out := range outs {
		if !*noTrunc {
			out.ID = utils.TruncateID(out.ID)
		}

		// Remove the leading / from the names
		for i := 0; i < len(out.Names); i++ {
			out.Names[i] = out.Names[i][1:]
		}

		if !*quiet {
			if !*noTrunc {
				out.Command = utils.Trunc(out.Command, 20)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t%s\t%s\t%s\t", out.ID, out.Image, out.Command, utils.HumanDuration(time.Now().Sub(time.Unix(out.Created, 0))), out.Status, displayablePorts(out.Ports), strings.Join(out.Names, ","))
			if *size {
				if out.SizeRootFs > 0 {
					fmt.Fprintf(w, "%s (virtual %s)\n", utils.HumanSize(out.SizeRw), utils.HumanSize(out.SizeRootFs))
				} else {
					fmt.Fprintf(w, "%s\n", utils.HumanSize(out.SizeRw))
				}
			} else {
				fmt.Fprint(w, "\n")
			}
		} else {
			fmt.Fprintln(w, out.ID)
		}
	}

	if !*quiet {
		w.Flush()
	}
	return nil
}

func (cli *DockerCli) CmdCommit(args ...string) error {
	cmd := Subcmd("commit", "[OPTIONS] CONTAINER [REPOSITORY[:TAG]]", "Create a new image from a container's changes")
	flComment := cmd.String("m", "", "Commit message")
	flAuthor := cmd.String("author", "", "Author (eg. \"John Hannibal Smith <hannibal@a-team.com>\"")
	flConfig := cmd.String("run", "", "Config automatically applied when the image is run. "+`(ex: {"Cmd": ["cat", "/world"], "PortSpecs": ["22"]}')`)
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	name := cmd.Arg(0)
	repository, tag := utils.ParseRepositoryTag(cmd.Arg(1))

	if name == "" {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	v.Set("container", name)
	v.Set("repo", repository)
	v.Set("tag", tag)
	v.Set("comment", *flComment)
	v.Set("author", *flAuthor)
	var config *Config
	if *flConfig != "" {
		config = &Config{}
		if err := json.Unmarshal([]byte(*flConfig), config); err != nil {
			return err
		}
	}
	body, _, err := cli.call("POST", "/commit?"+v.Encode(), config)
	if err != nil {
		return err
	}

	apiID := &APIID{}
	err = json.Unmarshal(body, apiID)
	if err != nil {
		return err
	}

	fmt.Fprintf(cli.out, "%s\n", apiID.ID)
	return nil
}

func (cli *DockerCli) CmdEvents(args ...string) error {
	cmd := Subcmd("events", "[OPTIONS]", "Get real time events from the server")
	since := cmd.String("since", "", "Show events previously created (used for polling).")
	if err := cmd.Parse(args); err != nil {
		return nil
	}

	if cmd.NArg() != 0 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	if *since != "" {
		v.Set("since", *since)
	}

	if err := cli.stream("GET", "/events?"+v.Encode(), nil, cli.out, nil); err != nil {
		return err
	}
	return nil
}

func (cli *DockerCli) CmdExport(args ...string) error {
	cmd := Subcmd("export", "CONTAINER", "Export the contents of a filesystem as a tar archive")
	if err := cmd.Parse(args); err != nil {
		return nil
	}

	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	if err := cli.stream("GET", "/containers/"+cmd.Arg(0)+"/export", nil, cli.out, nil); err != nil {
		return err
	}
	return nil
}

func (cli *DockerCli) CmdDiff(args ...string) error {
	cmd := Subcmd("diff", "CONTAINER", "Inspect changes on a container's filesystem")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	body, _, err := cli.call("GET", "/containers/"+cmd.Arg(0)+"/changes", nil)
	if err != nil {
		return err
	}

	changes := []Change{}
	err = json.Unmarshal(body, &changes)
	if err != nil {
		return err
	}
	for _, change := range changes {
		fmt.Fprintf(cli.out, "%s\n", change.String())
	}
	return nil
}

func (cli *DockerCli) CmdLogs(args ...string) error {
	cmd := Subcmd("logs", "CONTAINER", "Fetch the logs of a container")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}
	name := cmd.Arg(0)
	body, _, err := cli.call("GET", "/containers/"+name+"/json", nil)
	if err != nil {
		return err
	}

	container := &Container{}
	err = json.Unmarshal(body, container)
	if err != nil {
		return err
	}

	if err := cli.hijack("POST", "/containers/"+name+"/attach?logs=1&stdout=1&stderr=1", container.Config.Tty, nil, cli.out, cli.err, nil); err != nil {
		return err
	}
	return nil
}

func (cli *DockerCli) CmdAttach(args ...string) error {
	cmd := Subcmd("attach", "[OPTIONS] CONTAINER", "Attach to a running container")
	noStdin := cmd.Bool("nostdin", false, "Do not attach stdin")
	proxy := cmd.Bool("sig-proxy", true, "Proxify all received signal to the process (even in non-tty mode)")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}
	name := cmd.Arg(0)
	body, _, err := cli.call("GET", "/containers/"+name+"/json", nil)
	if err != nil {
		return err
	}

	container := &Container{}
	err = json.Unmarshal(body, container)
	if err != nil {
		return err
	}

	if !container.State.Running {
		return fmt.Errorf("Impossible to attach to a stopped container, start it first")
	}

	if container.Config.Tty && cli.isTerminal {
		if err := cli.monitorTtySize(cmd.Arg(0)); err != nil {
			utils.Debugf("Error monitoring TTY size: %s", err)
		}
	}

	var in io.ReadCloser

	v := url.Values{}
	v.Set("stream", "1")
	if !*noStdin && container.Config.OpenStdin {
		v.Set("stdin", "1")
		in = cli.in
	}
	v.Set("stdout", "1")
	v.Set("stderr", "1")

	if *proxy && !container.Config.Tty {
		sigc := cli.forwardAllSignals(cmd.Arg(0))
		defer utils.StopCatch(sigc)
	}

	if err := cli.hijack("POST", "/containers/"+cmd.Arg(0)+"/attach?"+v.Encode(), container.Config.Tty, in, cli.out, cli.err, nil); err != nil {
		return err
	}
	return nil
}

func (cli *DockerCli) CmdSearch(args ...string) error {
	cmd := Subcmd("search", "TERM", "Search the docker index for images")
	noTrunc := cmd.Bool("notrunc", false, "Don't truncate output")
	trusted := cmd.Bool("trusted", false, "Only show trusted builds")
	stars := cmd.Int("stars", 0, "Only displays with at least xxx stars")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 1 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	v.Set("term", cmd.Arg(0))
	body, _, err := cli.call("GET", "/images/search?"+v.Encode(), nil)
	if err != nil {
		return err
	}

	outs := []registry.SearchResult{}
	err = json.Unmarshal(body, &outs)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cli.out, 10, 1, 3, ' ', 0)
	fmt.Fprintf(w, "NAME\tDESCRIPTION\tSTARS\tOFFICIAL\tTRUSTED\n")
	for _, out := range outs {
		if (*trusted && !out.IsTrusted) || (*stars > out.StarCount) {
			continue
		}
		desc := strings.Replace(out.Description, "\n", " ", -1)
		desc = strings.Replace(desc, "\r", " ", -1)
		if !*noTrunc && len(desc) > 45 {
			desc = utils.Trunc(desc, 42) + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t", out.Name, desc, out.StarCount)
		if out.IsOfficial {
			fmt.Fprint(w, "[OK]")

		}
		fmt.Fprint(w, "\t")
		if out.IsTrusted {
			fmt.Fprint(w, "[OK]")
		}
		fmt.Fprint(w, "\n")
	}
	w.Flush()
	return nil
}

// Ports type - Used to parse multiple -p flags
type ports []int

// AttachOpts stores arguments to 'docker run -a', eg. which streams to attach to
type AttachOpts map[string]bool

func NewAttachOpts() AttachOpts {
	return make(AttachOpts)
}

func (opts AttachOpts) String() string {
	// Cast to underlying map type to avoid infinite recursion
	return fmt.Sprintf("%v", map[string]bool(opts))
}

func (opts AttachOpts) Set(val string) error {
	if val != "stdin" && val != "stdout" && val != "stderr" {
		return fmt.Errorf("Unsupported stream name: %s", val)
	}
	opts[val] = true
	return nil
}

func (opts AttachOpts) Get(val string) bool {
	if res, exists := opts[val]; exists {
		return res
	}
	return false
}

// PathOpts stores a unique set of absolute paths
type PathOpts map[string]struct{}

func NewPathOpts() PathOpts {
	return make(PathOpts)
}

func (opts PathOpts) String() string {
	return fmt.Sprintf("%v", map[string]struct{}(opts))
}

func (opts PathOpts) Set(val string) error {
	var containerPath string

	splited := strings.SplitN(val, ":", 2)
	if len(splited) == 1 {
		containerPath = splited[0]
		val = filepath.Clean(splited[0])
	} else {
		containerPath = splited[1]
		val = fmt.Sprintf("%s:%s", splited[0], filepath.Clean(splited[1]))
	}

	if !filepath.IsAbs(containerPath) {
		utils.Debugf("%s is not an absolute path", containerPath)
		return fmt.Errorf("%s is not an absolute path", containerPath)
	}
	opts[val] = struct{}{}
	return nil
}

func (cli *DockerCli) CmdTag(args ...string) error {
	cmd := Subcmd("tag", "[OPTIONS] IMAGE REPOSITORY[:TAG]", "Tag an image into a repository")
	force := cmd.Bool("f", false, "Force")
	if err := cmd.Parse(args); err != nil {
		return nil
	}
	if cmd.NArg() != 2 && cmd.NArg() != 3 {
		cmd.Usage()
		return nil
	}

	v := url.Values{}
	repository, tag := utils.ParseRepositoryTag(cmd.Arg(1))

	v.Set("repo", repository)
	v.Set("tag", tag)

	if *force {
		v.Set("force", "1")
	}

	if _, _, err := cli.call("POST", "/images/"+cmd.Arg(0)+"/tag?"+v.Encode(), nil); err != nil {
		return err
	}
	return nil
}

func (cli *DockerCli) CmdRun(args ...string) error {
	config, hostConfig, cmd, err := ParseRun(args, nil)
	if err != nil {
		return err
	}
	if config.Image == "" {
		cmd.Usage()
		return nil
	}

	flRm := cmd.Lookup("rm")
	autoRemove, _ := strconv.ParseBool(flRm.Value.String())

	flSigProxy := cmd.Lookup("sig-proxy")
	sigProxy, _ := strconv.ParseBool(flSigProxy.Value.String())
	flName := cmd.Lookup("name")
	if config.Tty {
		sigProxy = false
	}

	var containerIDFile *os.File
	if len(hostConfig.ContainerIDFile) > 0 {
		if _, err := ioutil.ReadFile(hostConfig.ContainerIDFile); err == nil {
			return fmt.Errorf("cid file found, make sure the other container isn't running or delete %s", hostConfig.ContainerIDFile)
		}
		containerIDFile, err = os.Create(hostConfig.ContainerIDFile)
		if err != nil {
			return fmt.Errorf("failed to create the container ID file: %s", err)
		}
		defer containerIDFile.Close()
	}
	containerValues := url.Values{}
	name := flName.Value.String()
	if name != "" {
		containerValues.Set("name", name)
	}

	//create the container
	body, statusCode, err := cli.call("POST", "/containers/create?"+containerValues.Encode(), config)
	//if image not found try to pull it
	if statusCode == 404 {
		_, tag := utils.ParseRepositoryTag(config.Image)
		if tag == "" {
			tag = DEFAULTTAG
		}

		fmt.Fprintf(cli.err, "Unable to find image '%s' (tag: %s) locally\n", config.Image, tag)

		v := url.Values{}
		repos, tag := utils.ParseRepositoryTag(config.Image)
		v.Set("fromImage", repos)
		v.Set("tag", tag)

		// Resolve the Repository name from fqn to endpoint + name
		var endpoint string
		endpoint, _, err = registry.ResolveRepositoryName(repos)
		if err != nil {
			return err
		}

		// Load the auth config file, to be able to pull the image
		cli.LoadConfigFile()

		// Resolve the Auth config relevant for this server
		authConfig := cli.configFile.ResolveAuthConfig(endpoint)
		buf, err := json.Marshal(authConfig)
		if err != nil {
			return err
		}

		registryAuthHeader := []string{
			base64.URLEncoding.EncodeToString(buf),
		}
		err = cli.stream("POST", "/images/create?"+v.Encode(), nil, cli.err, map[string][]string{
			"X-Registry-Auth": registryAuthHeader,
		})
		if err != nil {
			return err
		}
		body, _, err = cli.call("POST", "/containers/create?"+containerValues.Encode(), config)
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	runResult := &APIRun{}
	err = json.Unmarshal(body, runResult)
	if err != nil {
		return err
	}

	for _, warning := range runResult.Warnings {
		fmt.Fprintf(cli.err, "WARNING: %s\n", warning)
	}
	if len(hostConfig.ContainerIDFile) > 0 {
		if _, err = containerIDFile.WriteString(runResult.ID); err != nil {
			return fmt.Errorf("failed to write the container ID to the file: %s", err)
		}
	}

	if sigProxy {
		sigc := cli.forwardAllSignals(runResult.ID)
		defer utils.StopCatch(sigc)
	}

	var (
		wait  chan struct{}
		errCh chan error
	)

	if !config.AttachStdout && !config.AttachStderr {
		// Make this asynchrone in order to let the client write to stdin before having to read the ID
		wait = make(chan struct{})
		go func() {
			defer close(wait)
			fmt.Fprintf(cli.out, "%s\n", runResult.ID)
		}()
	}

	hijacked := make(chan bool)

	if config.AttachStdin || config.AttachStdout || config.AttachStderr {

		v := url.Values{}
		v.Set("stream", "1")
		var out, stderr io.Writer
		var in io.ReadCloser

		if config.AttachStdin {
			v.Set("stdin", "1")
			in = cli.in
		}
		if config.AttachStdout {
			v.Set("stdout", "1")
			out = cli.out
		}
		if config.AttachStderr {
			v.Set("stderr", "1")
			if config.Tty {
				stderr = cli.out
			} else {
				stderr = cli.err
			}
		}

		errCh = utils.Go(func() error {
			return cli.hijack("POST", "/containers/"+runResult.ID+"/attach?"+v.Encode(), config.Tty, in, out, stderr, hijacked)
		})
	} else {
		close(hijacked)
	}

	// Acknowledge the hijack before starting
	select {
	case <-hijacked:
	case err := <-errCh:
		if err != nil {
			utils.Debugf("Error hijack: %s", err)
			return err
		}
	}

	//start the container
	if _, _, err = cli.call("POST", "/containers/"+runResult.ID+"/start", hostConfig); err != nil {
		return err
	}

	if (config.AttachStdin || config.AttachStdout || config.AttachStderr) && config.Tty && cli.isTerminal {
		if err := cli.monitorTtySize(runResult.ID); err != nil {
			utils.Errorf("Error monitoring TTY size: %s\n", err)
		}
	}

	if errCh != nil {
		if err := <-errCh; err != nil {
			utils.Debugf("Error hijack: %s", err)
			return err
		}
	}

	if !config.AttachStdout && !config.AttachStderr {
		// Detached mode
		<-wait
	} else {
		running, status, err := getExitCode(cli, runResult.ID)
		if err != nil {
			return err
		}
		if autoRemove {
			if running {
				return fmt.Errorf("Impossible to auto-remove a detached container")
			}
			// Wait for the process to
			if _, _, err := cli.call("POST", "/containers/"+runResult.ID+"/wait", nil); err != nil {
				return err
			}
			if _, _, err := cli.call("DELETE", "/containers/"+runResult.ID, nil); err != nil {
				return err
			}
		}
		if status != 0 {
			return &utils.StatusError{Status: status}
		}
	}

	return nil
}

func (cli *DockerCli) CmdCp(args ...string) error {
	cmd := Subcmd("cp", "CONTAINER:RESOURCE HOSTPATH", "Copy files/folders from the RESOURCE to the HOSTPATH")
	if err := cmd.Parse(args); err != nil {
		return nil
	}

	if cmd.NArg() != 2 {
		cmd.Usage()
		return nil
	}

	var copyData APICopy
	info := strings.Split(cmd.Arg(0), ":")

	if len(info) != 2 {
		return fmt.Errorf("Error: Resource not specified")
	}

	copyData.Resource = info[1]
	copyData.HostPath = cmd.Arg(1)

	data, statusCode, err := cli.call("POST", "/containers/"+info[0]+"/copy", copyData)
	if err != nil {
		return err
	}

	if statusCode == 200 {
		r := bytes.NewReader(data)
		if err := archive.Untar(r, copyData.HostPath); err != nil {
			return err
		}
	}
	return nil
}

func (cli *DockerCli) call(method, path string, data interface{}) ([]byte, int, error) {
	var params io.Reader
	if data != nil {
		buf, err := json.Marshal(data)
		if err != nil {
			return nil, -1, err
		}
		params = bytes.NewBuffer(buf)
	}

	// fixme: refactor client to support redirect
	re := regexp.MustCompile("/+")
	path = re.ReplaceAllString(path, "/")

	req, err := http.NewRequest(method, fmt.Sprintf("/v%g%s", APIVERSION, path), params)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("User-Agent", "Docker-Client/"+VERSION)
	req.Host = cli.addr
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	} else if method == "POST" {
		req.Header.Set("Content-Type", "plain/text")
	}
	dial, err := net.Dial(cli.proto, cli.addr)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, -1, ErrConnectionRefused
		}
		return nil, -1, err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	resp, err := clientconn.Do(req)
	defer clientconn.Close()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, -1, ErrConnectionRefused
		}
		return nil, -1, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, -1, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		if len(body) == 0 {
			return nil, resp.StatusCode, fmt.Errorf("Error: %s", http.StatusText(resp.StatusCode))
		}
		return nil, resp.StatusCode, fmt.Errorf("Error: %s", body)
	}
	return body, resp.StatusCode, nil
}

func (cli *DockerCli) stream(method, path string, in io.Reader, out io.Writer, headers map[string][]string) error {
	if (method == "POST" || method == "PUT") && in == nil {
		in = bytes.NewReader([]byte{})
	}

	// fixme: refactor client to support redirect
	re := regexp.MustCompile("/+")
	path = re.ReplaceAllString(path, "/")

	req, err := http.NewRequest(method, fmt.Sprintf("/v%g%s", APIVERSION, path), in)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Docker-Client/"+VERSION)
	req.Host = cli.addr
	if method == "POST" {
		req.Header.Set("Content-Type", "plain/text")
	}

	if headers != nil {
		for k, v := range headers {
			req.Header[k] = v
		}
	}

	dial, err := net.Dial(cli.proto, cli.addr)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("Can't connect to docker daemon. Is 'docker -d' running on this host?")
		}
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	resp, err := clientconn.Do(req)
	defer clientconn.Close()
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("Can't connect to docker daemon. Is 'docker -d' running on this host?")
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if len(body) == 0 {
			return fmt.Errorf("Error :%s", http.StatusText(resp.StatusCode))
		}
		return fmt.Errorf("Error: %s", body)
	}

	if matchesContentType(resp.Header.Get("Content-Type"), "application/json") {
		return utils.DisplayJSONMessagesStream(resp.Body, out)
	} else {
		if _, err := io.Copy(out, resp.Body); err != nil {
			return err
		}
	}
	return nil
}

func (cli *DockerCli) hijack(method, path string, setRawTerminal bool, in io.ReadCloser, stdout, stderr io.Writer, started chan bool) error {
	// fixme: refactor client to support redirect
	re := regexp.MustCompile("/+")
	path = re.ReplaceAllString(path, "/")

	req, err := http.NewRequest(method, fmt.Sprintf("/v%g%s", APIVERSION, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Docker-Client/"+VERSION)
	req.Header.Set("Content-Type", "plain/text")
	req.Host = cli.addr

	dial, err := net.Dial(cli.proto, cli.addr)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return fmt.Errorf("Can't connect to docker daemon. Is 'docker -d' running on this host?")
		}
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server hijacks the connection, error 'connection closed' expected
	clientconn.Do(req)

	rwc, br := clientconn.Hijack()
	defer rwc.Close()

	if started != nil {
		started <- true
	}

	var receiveStdout chan error

	if stdout != nil {
		receiveStdout = utils.Go(func() (err error) {
			// When TTY is ON, use regular copy
			if setRawTerminal {
				_, err = io.Copy(stdout, br)
			} else {
				_, err = utils.StdCopy(stdout, stderr, br)
			}
			utils.Debugf("[hijack] End of stdout")
			return err
		})
	}

	if in != nil && setRawTerminal && cli.isTerminal && os.Getenv("NORAW") == "" {
		oldState, err := term.SetRawTerminal(cli.terminalFd)
		if err != nil {
			return err
		}
		defer term.RestoreTerminal(cli.terminalFd, oldState)
	}

	sendStdin := utils.Go(func() error {
		if in != nil {
			io.Copy(rwc, in)
			utils.Debugf("[hijack] End of stdin")
		}
		if tcpc, ok := rwc.(*net.TCPConn); ok {
			if err := tcpc.CloseWrite(); err != nil {
				utils.Errorf("Couldn't send EOF: %s\n", err)
			}
		} else if unixc, ok := rwc.(*net.UnixConn); ok {
			if err := unixc.CloseWrite(); err != nil {
				utils.Errorf("Couldn't send EOF: %s\n", err)
			}
		}
		// Discard errors due to pipe interruption
		return nil
	})

	if stdout != nil {
		if err := <-receiveStdout; err != nil {
			utils.Errorf("Error receiveStdout: %s", err)
			return err
		}
	}

	if !cli.isTerminal {
		if err := <-sendStdin; err != nil {
			utils.Errorf("Error sendStdin: %s", err)
			return err
		}
	}
	return nil

}

func (cli *DockerCli) getTtySize() (int, int) {
	if !cli.isTerminal {
		return 0, 0
	}
	ws, err := term.GetWinsize(cli.terminalFd)
	if err != nil {
		utils.Errorf("Error getting size: %s", err)
		if ws == nil {
			return 0, 0
		}
	}
	return int(ws.Height), int(ws.Width)
}

func (cli *DockerCli) resizeTty(id string) {
	height, width := cli.getTtySize()
	if height == 0 && width == 0 {
		return
	}
	v := url.Values{}
	v.Set("h", strconv.Itoa(height))
	v.Set("w", strconv.Itoa(width))
	if _, _, err := cli.call("POST", "/containers/"+id+"/resize?"+v.Encode(), nil); err != nil {
		utils.Errorf("Error resize: %s", err)
	}
}

func (cli *DockerCli) monitorTtySize(id string) error {
	cli.resizeTty(id)

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGWINCH)
	go func() {
		for _ = range sigchan {
			cli.resizeTty(id)
		}
	}()
	return nil
}

func Subcmd(name, signature, description string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.Usage = func() {
		// FIXME: use custom stdout or return error
		fmt.Fprintf(os.Stdout, "\nUsage: docker %s %s\n\n%s\n\n", name, signature, description)
		flags.PrintDefaults()
	}
	return flags
}

func (cli *DockerCli) LoadConfigFile() (err error) {
	cli.configFile, err = auth.LoadConfig(os.Getenv("HOME"))
	if err != nil {
		fmt.Fprintf(cli.err, "WARNING: %s\n", err)
	}
	return err
}

func waitForExit(cli *DockerCli, containerId string) (int, error) {
	body, _, err := cli.call("POST", "/containers/"+containerId+"/wait", nil)
	if err != nil {
		// If we can't connect, then the daemon probably died.
		if err != ErrConnectionRefused {
			return -1, err
		}
		return -1, nil
	}

	var out APIWait
	if err := json.Unmarshal(body, &out); err != nil {
		return -1, err
	}
	return out.StatusCode, nil
}

// getExitCode perform an inspect on the container. It returns
// the running state and the exit code.
func getExitCode(cli *DockerCli, containerId string) (bool, int, error) {
	body, _, err := cli.call("GET", "/containers/"+containerId+"/json", nil)
	if err != nil {
		// If we can't connect, then the daemon probably died.
		if err != ErrConnectionRefused {
			return false, -1, err
		}
		return false, -1, nil
	}
	c := &Container{}
	if err := json.Unmarshal(body, c); err != nil {
		return false, -1, err
	}
	return c.State.Running, c.State.ExitCode, nil
}

func NewDockerCli(in io.ReadCloser, out, err io.Writer, proto, addr string) *DockerCli {
	var (
		isTerminal = false
		terminalFd uintptr
	)

	if in != nil {
		if file, ok := in.(*os.File); ok {
			terminalFd = file.Fd()
			isTerminal = term.IsTerminal(terminalFd)
		}
	}

	if err == nil {
		err = out
	}
	return &DockerCli{
		proto:      proto,
		addr:       addr,
		in:         in,
		out:        out,
		err:        err,
		isTerminal: isTerminal,
		terminalFd: terminalFd,
	}
}

type DockerCli struct {
	proto      string
	addr       string
	configFile *auth.ConfigFile
	in         io.ReadCloser
	out        io.Writer
	err        io.Writer
	isTerminal bool
	terminalFd uintptr
}
