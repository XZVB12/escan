package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fatih/structs"
	"github.com/gorilla/mux"
	"github.com/malice-plugins/go-plugin-utils/database/elasticsearch"
	"github.com/malice-plugins/go-plugin-utils/utils"
	"github.com/parnurzeal/gorequest"
	"github.com/urfave/cli"
)

var (
	// Version stores the plugin's version
	Version string
	// BuildTime stores the plugin's build time
	BuildTime string

	path string
)

const (
	name     = "escan"
	category = "av"
)

type pluginResults struct {
	ID   string      `json:"id" structs:"id,omitempty"`
	Data ResultsData `json:"escan" structs:"escan"`
}

// EScan json object
type EScan struct {
	Results ResultsData `json:"escan"`
}

// ResultsData json object
type ResultsData struct {
	Infected bool   `json:"infected" structs:"infected"`
	Result   string `json:"result" structs:"result"`
	Engine   string `json:"engine" structs:"engine"`
	Updated  string `json:"updated" structs:"updated"`
	MarkDown string `json:"markdown,omitempty" structs:"markdown,omitempty"`
	Error    string `json:"error,omitempty" structs:"error,omitempty"`
}

func assert(err error) {
	if err != nil {
		log.WithFields(log.Fields{
			"plugin":   name,
			"category": category,
			"path":     path,
		}).Fatal(err)
	}
}

func startZavService(ctx context.Context) error {
	// EScan needs to have the daemon started first
	_, err := utils.RunCommand(ctx, "/etc/init.d/zavd", "start", "--no-daemon")
	return err
}

// AvScan performs antivirus scan
func AvScan(timeout int) EScan {

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// start service
	assert(startZavService(ctx))

	results, err := utils.RunCommand(ctx, "zavcli", path)
	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
		"path":     path,
	}).Debug("EScan output: ", results)

	if err != nil {
		// EScan exits with error status 11 if it finds a virus
		if err.Error() != "exit status 11" {
			log.WithFields(log.Fields{
				"plugin":   name,
				"category": category,
				"path":     path,
			}).Fatal(err)
		} else {
			err = nil
		}
	}

	return EScan{Results: ParseZonerOutput(results, err)}
}

// ParseZonerOutput convert escan output into ResultsData struct
func ParseZonerOutput(zonerout string, err error) ResultsData {

	if err != nil {
		return ResultsData{Error: err.Error()}
	}

	escan := ResultsData{Infected: false}

	lines := strings.Split(zonerout, "\n")

	// Extract Virus string
	for _, line := range lines {
		if len(line) != 0 {
			if strings.Contains(line, "INFECTED") {
				result := extractVirusName(line)
				if len(result) != 0 {
					escan.Result = result
					escan.Infected = true
				} else {
					return ResultsData{Error: fmt.Sprint("[ERROR] virus name extracted was empty: ", result)}
				}
			}
		}
	}
	escan.Engine = getEngine()
	escan.Updated = getUpdatedDate()

	return escan
}

// extractVirusName extracts Virus name from scan results string
func extractVirusName(line string) string {
	keyvalue := strings.Split(line, "INFECTED")
	return strings.Trim(strings.TrimSpace(keyvalue[1]), "[]")
}

func getEngine() string {
	var engine = ""

	// start service
	startZavService(context.Background())

	results, err := utils.RunCommand(nil, "zavcli", "--version-zavd")
	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
		"path":     path,
	}).Debug("EScan DB version: ", results)
	assert(err)

	for _, line := range strings.Split(results, "\n") {
		if len(line) != 0 {
			if strings.Contains(line, "ZAVDB version:") {
				engine = strings.TrimSpace(strings.TrimPrefix(line, "ZAVDB version:"))
			}
		}
	}
	return engine
}

func updateAV(ctx context.Context) error {
	fmt.Println("Updating EScan...")
	// EScan needs to have the daemon started first
	output, err := utils.RunCommand(nil, "/etc/init.d/zavd", "update")
	log.WithFields(log.Fields{
		"plugin":   name,
		"category": category,
		"path":     path,
	}).Debug("EScan update: ", output)
	assert(err)

	// Update UPDATED file
	t := time.Now().Format("20060102")
	err = ioutil.WriteFile("/opt/malice/UPDATED", []byte(t), 0644)
	return err
}

func getUpdatedDate() string {
	if _, err := os.Stat("/opt/malice/UPDATED"); os.IsNotExist(err) {
		return BuildTime
	}
	updated, err := ioutil.ReadFile("/opt/malice/UPDATED")
	utils.Assert(err)
	return string(updated)
}

func generateMarkDownTable(e EScan) string {
	var tplOut bytes.Buffer

	t := template.Must(template.New("").Parse(tpl))

	err := t.Execute(&tplOut, z)
	if err != nil {
		log.Println("executing template:", err)
	}

	return tplOut.String()
}

func printStatus(resp gorequest.Response, body string, errs []error) {
	fmt.Println(resp.Status)
}

func webService() {
	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/scan", webAvScan).Methods("POST")
	log.Info("web service listening on port :3993")
	log.Fatal(http.ListenAndServe(":3993", router))
}

func webAvScan(w http.ResponseWriter, r *http.Request) {

	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("malware")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Please supply a valid file to scan.")
		log.Error(err)
	}
	defer file.Close()

	log.Debug("Uploaded fileName: ", header.Filename)

	tmpfile, err := ioutil.TempFile("/malware", "web_")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(tmpfile.Name()) // clean up

	data, err := ioutil.ReadAll(file)
	assert(err)

	if _, err = tmpfile.Write(data); err != nil {
		log.Fatal(err)
	}
	if err = tmpfile.Close(); err != nil {
		log.Fatal(err)
	}

	// Do AV scan
	path = tmpfile.Name()
	escan := AvScan(60)

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(escan); err != nil {
		log.Fatal(err)
	}
}

func main() {

	var elastic string

	cli.AppHelpTemplate = utils.AppHelpTemplate
	app := cli.NewApp()

	app.Name = "escan"
	app.Author = "blacktop"
	app.Email = "https://github.com/blacktop"
	app.Version = Version + ", BuildTime: " + BuildTime
	app.Compiled, _ = time.Parse("20060102", BuildTime)
	app.Usage = "Malice eScan AntiVirus Plugin"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose, V",
			Usage: "verbose output",
		},
		cli.BoolFlag{
			Name:  "table, t",
			Usage: "output as Markdown table",
		},
		cli.BoolFlag{
			Name:   "callback, c",
			Usage:  "POST results to Malice webhook",
			EnvVar: "MALICE_ENDPOINT",
		},
		cli.BoolFlag{
			Name:   "proxy, x",
			Usage:  "proxy settings for Malice webhook endpoint",
			EnvVar: "MALICE_PROXY",
		},
		cli.StringFlag{
			Name:        "elasitcsearch",
			Value:       "",
			Usage:       "elasitcsearch address for Malice to store results",
			EnvVar:      "MALICE_ELASTICSEARCH",
			Destination: &elastic,
		},
		cli.IntFlag{
			Name:   "timeout",
			Value:  60,
			Usage:  "malice plugin timeout (in seconds)",
			EnvVar: "MALICE_TIMEOUT",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:    "update",
			Aliases: []string{"u"},
			Usage:   "Update virus definitions",
			Action: func(c *cli.Context) error {
				return updateAV(nil)
			},
		},
		{
			Name:  "web",
			Usage: "Create a EScan scan web service",
			Action: func(c *cli.Context) error {
				webService()
				return nil
			},
		},
	}
	app.Action = func(c *cli.Context) error {

		var err error

		if c.Bool("verbose") {
			log.SetLevel(log.DebugLevel)
		}

		if c.Args().Present() {
			path, err = filepath.Abs(c.Args().First())
			utils.Assert(err)

			if _, err := os.Stat(path); os.IsNotExist(err) {
				utils.Assert(err)
			}

			escan := AvScan(c.Int("timeout"))
			escan.Results.MarkDown = generateMarkDownTable(escan)

			// upsert into Database
			elasticsearch.InitElasticSearch(elastic)
			elasticsearch.WritePluginResultsToDatabase(elasticsearch.PluginResults{
				ID:       utils.Getopt("MALICE_SCANID", utils.GetSHA256(path)),
				Name:     name,
				Category: category,
				Data:     structs.Map(escan.Results),
			})

			if c.Bool("table") {
				fmt.Println(escan.Results.MarkDown)
			} else {
				escan.Results.MarkDown = ""
				zonerJSON, err := json.Marshal(escan)
				utils.Assert(err)
				if c.Bool("post") {
					request := gorequest.New()
					if c.Bool("proxy") {
						request = gorequest.New().Proxy(os.Getenv("MALICE_PROXY"))
					}
					request.Post(os.Getenv("MALICE_ENDPOINT")).
						Set("X-Malice-ID", utils.Getopt("MALICE_SCANID", utils.GetSHA256(path))).
						Send(string(zonerJSON)).
						End(printStatus)

					return nil
				}
				fmt.Println(string(zonerJSON))
			}
		} else {
			log.Fatal(fmt.Errorf("please supply a file to scan with malice/escan"))
		}
		return nil
	}

	err := app.Run(os.Args)
	utils.Assert(err)
}
