package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"sync/atomic"
	plainTmpl "text/template"

	"flag"
	"log"
)

var (
	errJobNotFound    = errors.New("job ID not found")
	errJobExists      = errors.New("job with the ID already exists")
	errJobNotFinished = errors.New("job not finished")

	errForbidden = errors.New("forbidden")
)

var (
	flagDebug      = flag.Bool("debug", false, "run in debug mode")
	flagCgroupExec = flag.String("cgroup_exec", "", "flag for internal use")
	flagLogDir     = flag.String("logdir", "/tmp/envdeploy_logs", "path to directory where to store logs")
	flagListenAddr = flag.String("listen", "127.0.0.1:80", "address for HTTP server to listen on")

	flagBasePath = flag.String("basepath", "", "path to be the root of envdeploy's web tree")
	flagConfFile = flag.String("conf", "deployables.json", "path to configuration file listing deployables")

	flagMockUser = flag.String("mockuser", "", "")
)

var (
	reJobPath     = regexp.MustCompile(`^/jobs/([a-z0-9-]+)(?:/(kill|remove|log)?)?$`)
	reGatewayPath = regexp.MustCompile(`^/enter/([a-z0-9-]+)/`)
	reDeployPath  = regexp.MustCompile(`^/deploy/([a-z0-9-]+)`)
)

func Link(path string) string {
	return *flagBasePath + path
}

var templatesPath string
var staticPath string

func initPaths() {
	dataDir := os.Getenv("ENVDEPLOY_DATA_DIR")
	if dataDir == "" {
		execPath, err := os.Executable()
		if err != nil {
			log.Panicf("os.Executable(): %s", err)
		}
		dataDir = path.Dir(execPath)
	}

	templatesPath = path.Join(dataDir, "templates")
	staticPath = path.Join(dataDir, "static")
}

type Deployable struct {
	ID           string `json:"ID"`
	Desc         string `json:"Desc"`
	LaunchScript string `json:"LaunchScript"`
	JobIDFormat  string `json:"JobIDFormat"`
}

var (
	templates   atomic.Value /* *template.Template */
	deployables atomic.Value /* []Deployables */
)

func readTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"sh":   Sh,
		"link": Link,
	}

	return template.New("").Funcs(funcMap).ParseGlob(templatesPath + "/*")
}

func loadTemplates() {
	new, err := readTemplates()
	if err != nil {
		log.Printf("error reading templates: %s", err)
		return
	}
	templates.Store(new)
}

func initTemplates() {
	loadTemplates()
	if templates.Load() == nil {
		log.Fatalf("failed to read templates")
	}
}

func tmpl() *template.Template {
	if *flagDebug {
		loadTemplates()
	}

	return templates.Load().(*template.Template)
}

func execTmpl(w http.ResponseWriter, name string, data interface{}) {
	err := tmpl().ExecuteTemplate(w, name, data)
	if err != nil {
		if *flagDebug {
			http.Error(w, err.Error(), 500)
		} else {
			http.Error(w, "internal server error", 500)
		}
		log.Printf("executing template %s: %s\n", name, err)
	}
}

func readDeployables() (ret []Deployable, err error) {
	contents, err := ioutil.ReadFile(*flagConfFile)
	if err != nil {
		return
	}
	err = json.Unmarshal(contents, &ret)
	return
}

func loadDeployables() {
	new, err := readDeployables()
	if err != nil {
		log.Printf("error reading %s: %s", *flagConfFile, err)
		return
	}
	deployables.Store(new)
}

func getDeployables() []Deployable {
	if *flagDebug {
		loadDeployables()
	}
	return deployables.Load().([]Deployable)
}

func initDeployables() {
	loadDeployables()
	if deployables.Load() == nil {
		log.Fatal("failed to read deployables")
	}
}

func lookupEnv(id string) *Deployable {
	depl := getDeployables()
	for _, d := range depl {
		if d.ID == id {
			return &d
		}
	}
	return nil
}

func listJobs(w http.ResponseWriter, r *http.Request, u user) {
	type JobInfo struct {
		ID, Owner string
		Running   bool
	}

	jobsInfo := []JobInfo{}
	jobs.RLock()
	for _, job := range jobs.m {
		if !u.CanAccessJob(job.Owner) {
			continue
		}
		jobsInfo = append(jobsInfo, JobInfo{
			job.ID,
			string(job.Owner),
			job.Started && !job.Finished,
		})
	}
	jobs.RUnlock()

	flashMessages := getFlashMessages(w, r)
	execTmpl(w, "list", map[string]interface{}{
		"flashMessages": flashMessages,
		"Jobs":          jobsInfo,
		"Deployables":   getDeployables(),
	})
}

func setFlashAndRedirect(w http.ResponseWriter, r *http.Request, url string, typ, msg string) {
	setFlashMessages(w, []flashMessage{{ID: typ, Args: []string{msg}}})
	http.Redirect(w, r, url, http.StatusFound)
}

func deploy(w http.ResponseWriter, r *http.Request, user_ user) {
	match := reDeployPath.FindStringSubmatch(r.URL.Path)
	if len(match) < 2 {
		http.Error(w, "environment not found", 404)
		return
	}

	d := lookupEnv(match[1])
	if d == nil {
		http.Error(w, "environment not found", 404)
		return
	}

	var jobID bytes.Buffer

	IDTmpl, err := plainTmpl.New("id").Parse(d.JobIDFormat)
	if err == nil || d.JobIDFormat == "" {
		var rid [2]byte
		rand.Read(rid[:])
		IDTmpl.Execute(&jobID, struct {
			Owner  string
			Random string
		}{string(user_), hex.EncodeToString(rid[:])})
	}
	if err != nil {
		log.Printf("error in job ID template for %s: %s\n", d.ID, err)
		http.Error(w, "internal server error", 500)
	}

	job, err := jobs.CreateJob(jobID.String(), user_)
	if err != nil {
		setFlashMessages(w, []flashMessage{{ID: "error", Args: []string{err.Error()}}})
		http.Redirect(w, r, Link("/"), http.StatusFound)
		return
	}

	envs := append(os.Environ(),
		fmt.Sprintf("WEB_BASE_PATH=%s/enter/%s", *flagBasePath, jobID.String()),
		fmt.Sprintf("JOB_OWNER=%s", user_),
	)
	job.Start(d.LaunchScript, envs, "/")
	setFlashAndRedirect(w, r, Link("/jobs/"+jobID.String()), "success", "Deployment successful")
}

func handleJob(w http.ResponseWriter, r *http.Request, u user) {
	match := reJobPath.FindStringSubmatch(r.URL.Path)
	if len(match) < 2 {
		http.Error(w, "job not found", 404)
		return
	}
	job := jobs.Lookup(match[1])
	if job == nil {
		http.Error(w, "job not found", 404)
		return
	}

	if len(match) >= 3 && match[2] != "" {
		if match[2] == "log" && r.Method == "GET" {
			http.ServeFile(w, r, job.StderrFn)
			return
		}

		if r.Method != "POST" {
			http.Error(w, "job action must be requested via POST", http.StatusBadRequest)
			return
		}

		switch match[2] {
		case "kill":
			no, err := strconv.Atoi(r.FormValue("signal"))
			if err != nil {
				http.Error(w, "no signal number", http.StatusBadRequest)
				return
			}
			job.SendSignal(no)
			setFlashAndRedirect(w, r, Link("/jobs/"+job.ID), "success",
				fmt.Sprintf("Job %s was sent signal %d", job.ID, no))
			return
		case "remove":
			err := jobs.Remove(job.ID)
			if err != nil {
				setFlashAndRedirect(w, r, Link("/jobs/"+job.ID), "error",
					fmt.Sprintf("Job %s could not be removed: %s", job.ID, err))
			} else {
				setFlashAndRedirect(w, r, Link("/"), "success", fmt.Sprintf("Job %s removed", job.ID))
			}
			return
		default:
			return
		}
	} else {
		if !u.CanAccessJob(job.Owner) {
			http.Error(w, "forbidden. you are not the job owner, neither are you an administrator", 403)
			return
		}

		flashMessages := getFlashMessages(w, r)
		execTmpl(w, "job_detail", map[string]interface{}{
			"flashMessages": flashMessages,
			"Job":           job,
		})
	}
}

func handleGateway(w http.ResponseWriter, r *http.Request, u user) {
	match := reGatewayPath.FindStringSubmatch(r.URL.Path)
	if len(match) != 2 {
		http.Error(w, "job not found", 404)
		return
	}
	job := jobs.Lookup(match[1])
	if job == nil {
		http.Error(w, "job not found", 404)
		return
	}
	if !u.CanAccessJob(job.Owner) {
		http.Error(w, "forbidden. you are not the job owner, neither are you an administrator", 403)
		return
	}

	job.ReverseProxy.ServeHTTP(w, r)
}

func getRequestUser(r *http.Request) user {
	if *flagMockUser != "" {
		return user(*flagMockUser)
	} else {
		return user(r.Header.Get("X-Forwarded-User"))
	}
}

func requireLogin(handler func(http.ResponseWriter, *http.Request, user)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		user := getRequestUser(r)

		if user == "" {
			http.Error(w, "forbidden", 403)
		} else {
			handler(w, r, user)
		}
	}
}

func main() {
	flag.Parse()

	if *flagCgroupExec != "" {
		internalCgroupExec(*flagCgroupExec)
	}

	initPaths()
	initTemplates()
	initDeployables()
	initCgroup()
	initUsers()
	initNet()

	mux := http.NewServeMux()

	mux.HandleFunc("/", requireLogin(listJobs))
	mux.HandleFunc("/deploy/", requireLogin(deploy))
	mux.HandleFunc("/jobs/", requireLogin(handleJob))
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticPath))))
	mux.HandleFunc("/enter/", requireLogin(handleGateway))

	h := http.StripPrefix(*flagBasePath, mux)
	err := http.ListenAndServe(*flagListenAddr, h)

	if err != nil {
		log.Fatal(err)
	}
}
