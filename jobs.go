package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"path"
	"sync"
	"time"
)

type job struct {
	ID     string
	Cgroup string

	Owner  user
	Public bool

	http.RoundTripper
	*httputil.ReverseProxy

	Stderr   *os.File
	StderrFn string

	Dialer *pinnedDialer

	Statem     sync.RWMutex
	Started    bool
	StartTime  time.Time
	Finished   bool
	FinishTime time.Time
}

type jobsMap struct {
	m map[string]*job
	sync.RWMutex
}

var jobs jobsMap = jobsMap{m: make(map[string]*job)}

func initJob(id string, owner user) (*job, error) {
	var err error

	cgroupPath := path.Join(cgroupJobsPath, id)
	createCgroup(cgroupPath)

	logDir := *flagLogDir
	err = os.MkdirAll(logDir, 0750)
	if err != nil {
		return nil, err
	}
	stderrFn := path.Join(
		logDir, /* TODO: sub-second time formatting is wrong */
		fmt.Sprintf("%s_%s", time.Now().Format("060102-15040507"), id),
	)
	stderr, err := os.Create(stderrFn)
	if err != nil {
		return nil, err
	}

	pd := CreatePinnedDialer()

	rt := &http.Transport{
		Dial:                pd.Dial,
		MaxIdleConns:        24,
		IdleConnTimeout:     1 * time.Hour,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
	}

	return &job{
		ID:           id,
		Cgroup:       cgroupPath,
		Owner:        owner,
		Dialer:       pd,
		Stderr:       stderr,
		StderrFn:     stderrFn,
		RoundTripper: rt,
		ReverseProxy: &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Path = *flagBasePath + req.URL.Path
				req.URL.Scheme = "http"
				req.URL.Host = "127.0.0.1:8000"
			},
			Transport: rt,
		},
	}, nil
}

func (jobs *jobsMap) CreateJob(id string, owner user) (ret *job, err error) {
	jobs.Lock()
	defer jobs.Unlock()

	if _, ok := jobs.m[id]; ok {
		return nil, errJobExists
	}

	ret, err = initJob(id, owner)
	if err != nil {
		return
	}

	jobs.m[id] = ret
	return
}

func (jobs *jobsMap) Lookup(id string) (ret *job) {
	jobs.RLock()
	defer jobs.RUnlock()
	ret, _ = jobs.m[id]
	return
}

func (jobs *jobsMap) Remove(id string) error {
	jobs.RLock()
	defer jobs.RUnlock()
	job, ok := jobs.m[id]
	if !ok {
		return errJobNotFound
	}
	if !job.IsFinished() {
		return errJobNotFinished
	}
	delete(jobs.m, id)
	return nil
}

func (j *job) Start(cmd string, env []string, dir string) {
	j.Statem.Lock()
	if j.Started {
		log.Printf("attempt to start already started job %s", j.ID)
		j.Statem.Unlock()
		return
	}
	j.Started = true
	j.StartTime = time.Now()
	j.Statem.Unlock()

	RunContainedWithDialer(j.ID, cmd, env, dir, j.Cgroup, j.Stderr, j.Dialer)

	/* wait for the job to get unpopulated, then Quit the pinnedDialer */
	go func() {
		<-waitForCgroupUnpopulated(j.Cgroup)

		j.Statem.Lock()
		j.Finished = true
		j.FinishTime = time.Now()
		j.Statem.Unlock()

		j.Dialer.Quit()
		err_str := Sh(fmt.Sprintf("rmdir %s", j.Cgroup))
		if err_str != "" {
			log.Println(err_str)
		}
	}()
}

func (j *job) IsFinished() bool {
	j.Statem.RLock()
	defer j.Statem.RUnlock()
	return j.Finished
}

func (j *job) SendSignal(sig int) {
	/* IMPROVE */
	err_str := Sh(fmt.Sprintf("kill -s %d $(find %s -name cgroup.procs | xargs cat | sort | head -n 1)", sig, j.Cgroup))
	if err_str != "" {
		log.Println(err_str)
	}
}
