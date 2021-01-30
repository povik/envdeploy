package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

var (
	cgroupBasePath = "/sys/fs/cgroup/unified"

	cgroupOurPath  = ""
	cgroupJobsPath = ""
)

func checkCgroupVersion() {
	p := cgroupBasePath

	var s syscall.Statfs_t
	if err := syscall.Statfs(p, &s); err != nil {
		log.Fatalf("could not statfs %s: %s", p, err)
	}
	if s.Type != unix.CGROUP2_SUPER_MAGIC {
		log.Fatalf("need cgroup v2 mounted at %s", p)
	}
}

func getOurCgroup() (string, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	defer f.Close()

	reader := bufio.NewReader(f)

	for {
		l, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(l, "0:") {
			s := strings.SplitN(l, ":", 3)
			if len(s) == 3 {
				return strings.TrimSpace(s[2]), nil
			}
		}
	}

	return "", fmt.Errorf("entry not found")
}

func cgroupAttach(p string) {
	err := ioutil.WriteFile(path.Join(p, "cgroup.procs"), []byte("0\n"), 0755)
	if err != nil {
		log.Fatalf("failed to attach to new cgroup: %s", err)
	}
}

func initCgroup() {
	checkCgroupVersion()

	p, err := getOurCgroup()
	if err != nil {
		log.Fatalf("could not read our cgroup path: %s", err)
	}
	log.Printf("our start-up cgroup is %s", p)
	cgroupOurPath = path.Join(cgroupBasePath, p, "server")
	cgroupJobsPath = path.Join(cgroupBasePath, p, "jobs")

	err = os.MkdirAll(cgroupOurPath, 0755)
	if err != nil {
		log.Fatalf("could not create cgroup subgroup: %s", err)
	}
	err = os.MkdirAll(cgroupJobsPath, 0755)
	if err != nil {
		log.Fatalf("could not create cgroup subgroup: %s", err)
	}

	cgroupAttach(cgroupOurPath)
}

func internalCgroupExec(flagArg string) {
	otherArgs := flag.Args()
	cgroupAttach(flagArg)
	err := syscall.Exec(otherArgs[0], otherArgs[1:], os.Environ())
	log.Fatalf("envdeploy: failed to exec %s: %s\n", otherArgs[0], err)
}

func startProcessInCgroup(path string, argv []string, env []string, dir string, cgroupPath string, stderr *os.File) (proc *os.Process, err error) {
	argv = append([]string{"envdeploy", "-cgroup_exec", cgroupPath, path}, argv...)

	proc, err = os.StartProcess("/proc/self/exe", argv, &os.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []*os.File{devNull, stderr, stderr},
	})

	return
}

func isCgroupPopulated(dir string) (ret bool) {
	ret = true

	eventsFn := path.Join(dir, "cgroup.events")
	f, err := os.OpenFile(eventsFn, os.O_RDONLY, 0)
	if err != nil {
		log.Printf("can't read %s: %v\n", eventsFn, err)
		if err == os.ErrNotExist {
			ret = false
		}
		return
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		var val int
		nparsed, _ := fmt.Sscanf(s.Text(), "populated %d", &val)
		if nparsed > 0 {
			ret = val != 0
		}
	}

	return
}

func waitForCgroupUnpopulated(dir string) chan interface{} {
	ret := make(chan interface{})
	go func() {
		defer close(ret)

		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			log.Println(err)
			return
		}
		defer watcher.Close()
		err = watcher.Add(path.Join(dir, "cgroup.events"))

		for {
			if !isCgroupPopulated(dir) {
				return
			}

			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				_ = event
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println(err)
			}
		}
	}()
	return ret
}

func createCgroup(dir string) {
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		log.Printf("could not create cgroup: %s", err)
	}
}
