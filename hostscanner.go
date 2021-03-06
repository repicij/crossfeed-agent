package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/jack-dds/webanalyze"
	"github.com/lib/pq"
)

var wappalyzeAppsPath = "output/hostscanner/apps.json"

type Request struct {
	Filters map[string][]string
	Greps   []string
	Request struct {
		Method  string
		Uri     string
		Headers map[string]string
		Body    string
	}
}

func fetchHosts(args []string) {
	log.SetPrefix("[hostscanner] ")
	if len(args) < 1 {
		log.Fatal("Please provide the config file or a single path.")
	}
	switch args[0] {
	case "initWappalyzer":
		initWappalyzer()
	case "jsonInput":
		if len(args) < 2 {
			log.Fatal("Please provide the input.")
		}
		initWithJSONInput(args[1], args[2])
	default:
		initHostScan(args[0], args[1], nil)
	}
}

func initWithJSONInput(jsonInput string, taskID string) {
	var request Request
	err := json.Unmarshal([]byte(strings.Trim(jsonInput, `'`)), &request)
	handleError(err)
	initHostScan("", taskID, &request)
}

func initHostScan(input string, taskID string, megRequest *Request) {
	var (
		scanID           = getTimestamp(true)
		rootPath         = "output/hostscanner/" + scanID + "/"
		hostsPath        = rootPath + "hosts.txt"
		pathsPath        = rootPath + "paths.txt"
		outPath          = rootPath + "megoutput/"
		configPath       = "config/hostscanner/"
		megTimeoutLength = 60
	)

	var paths []string

	if megRequest != nil {
		paths = append(paths, megRequest.Request.Uri)
	} else if strings.HasPrefix(input, "/") { // Treat as single path
		paths = append(paths, input)
	} else { // Find input config file
		file, err := os.Open(path.Join(configPath, strings.Replace(input, ".", "", -1)))
		if err != nil {
			log.Fatal("Unable to find config file " + input)
		}
		defer file.Close()

		lineScanner := bufio.NewScanner(file)
		for lineScanner.Scan() {
			paths = append(paths, lineScanner.Text())
		}
	}

	query := `SELECT name, ports FROM "Domains" WHERE ports LIKE '%80%' OR ports LIKE '%443%';`
	if megRequest != nil { // Format query to match request filters
		query = `SELECT name, ports FROM "Domains" WHERE `
		var andConds []string
		for filter, vals := range megRequest.Filters {
			if len(vals) == 0 {
				continue
			}
			var orConds []string
			for _, val := range vals {
				orConds = append(orConds, fmt.Sprintf("%s LIKE '%%%s%%'", filter, val))
			}
			andConds = append(andConds, "("+strings.Join(orConds, " OR ")+")")
		}
		query += strings.Join(andConds, " AND ")
	}
	rows, err := db.Query(query)
	handleError(err)
	defer rows.Close()

	_, err = exec.Command("mkdir", rootPath).Output()
	handleError(err)

	f, err := os.Create(hostsPath)
	handleError(err)

	var name string
	var ports string
	var count int
	for rows.Next() { // Prepend http/https to all hosts
		err := rows.Scan(&name, &ports)
		handleError(err)
		protocol := "http://"
		if strings.Contains(ports, "443") {
			protocol = "https://"
		}
		_, err = f.WriteString(fmt.Sprintf("%s%s\n", protocol, name))
		handleError(err)
		count++
	}

	f.Close()

	f, err = os.Create(pathsPath)
	handleError(err)

	for _, path := range paths {
		_, err = f.WriteString(path + "\n")
		handleError(err)
	}

	f.Close()

	log.Println(fmt.Sprintf("Beginning host scan for %d paths on %d domains", len(paths), count))

	args := []string{"-c", "30", "-v"}

	if megRequest != nil {
		args = append(args, "-X")
		args = append(args, megRequest.Request.Method)
		for name, val := range megRequest.Request.Headers {
			if name == "Host" || name == "User-Agent" {
				if strings.Contains(val, "{host}") {
					continue
				}
				args = append(args, "--rawhttp")
			}
			args = append(args, "-H")
			args = append(args, fmt.Sprintf("%s: %s", name, val))
		}
		if megRequest.Request.Body != "" {
			args = append(args, "-b")
			args = append(args, megRequest.Request.Body)
		}
	}

	args = append(args, pathsPath)
	args = append(args, hostsPath)
	args = append(args, outPath)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(megTimeoutLength)*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "meg", args...)

	// Start the command after having set up the pipe
	stdout, err := cmd.StdoutPipe()
	err = cmd.Start()
	handleError(err)

	cur := 0
	in := bufio.NewScanner(stdout)
	for in.Scan() {
		cur++
		updateTaskPercentage(taskID, (100*cur)/count)
		log.Println(fmt.Sprintf("(%d/%d) %s", cur, count, in.Text()))
	}

	if ctx.Err() == context.DeadlineExceeded { // If command timed out, still process results
		log.Println(fmt.Sprintf("Command meg timed out after %d minutes, continuing.", megTimeoutLength))
	} else {
		err = in.Err()
		handleError(err)
	}

	if megRequest != nil { // Grep responses if a custom request
		var alertOutput []string
		for _, grep := range megRequest.Greps {
			cmd := exec.Command("grep", "-Hri", grep, outPath)

			stdout, err := cmd.StdoutPipe()
			err = cmd.Start()
			handleError(err)

			in := bufio.NewScanner(stdout)
			for in.Scan() {
				text := in.Text()
				if !strings.Contains(text, "/") || !strings.Contains(text, ":") || strings.Contains(text, "megoutput//index:") {
					continue
				}
				domain := strings.Split(strings.Replace(text, outPath+"/", "", 1), "/")[0]
				match := strings.SplitN(text, ":", 2)[1]
				alertLine := fmt.Sprintf("Domain %s matched string %s: %s", domain, grep, match)
				log.Println(alertLine)
				alertOutput = append(alertOutput, alertLine)
			}
		}
		if len(alertOutput) > 0 {
			alertText := fmt.Sprintf("%d results found for request to %s:\n", len(alertOutput), megRequest.Request.Uri)
			updateTaskOutput(fmt.Sprintf("Host Scan for %s", megRequest.Request.Uri), alertText+strings.Join(alertOutput, "\n"), 3)
		}
	}

	log.Println(fmt.Sprintf("Finished host scan for %d paths on %d domains", len(paths), count))

	if input == "/" { // Wappalyze results if index page
		wappalyzeResults(scanID, rootPath, outPath)
	}

}

func initWappalyzer() {
	err := webanalyze.DownloadFile(webanalyze.WappalyzerURL, wappalyzeAppsPath)
	handleError(err)
	log.Println("app definition file updated from ", webanalyze.WappalyzerURL)
}

func wappalyzeResults(scanID string, rootPath string, outPath string) {
	workers := 4
	crawlCount := 0
	searchSubdomain := false
	file, err := os.Open(path.Join(outPath, "index"))
	handleError(err)
	defer file.Close()

	results, err := webanalyze.Init(workers, file, wappalyzeAppsPath, crawlCount, searchSubdomain)
	handleError(err)

	var hostsArray []string
	var servicesArray []string

	for result := range results {
		if result.Error != nil {
			log.Printf("[-] Error for %v: %v", result.Host, result.Error)
			continue
		}

		if len(result.Matches) == 0 {
			continue
		}

		hostName := strings.Replace(strings.Split(result.Host, "//")[1], "/", "", -1)
		hostsArray = append(hostsArray, hostName)

		results := ""
		for i, a := range result.Matches {

			var categories []string

			for _, cid := range a.App.Cats {
				categories = append(categories, webanalyze.AppDefs.Cats[string(cid)].Name)
			}

			if i != 0 {
				results += ", "
			}

			results += a.AppName

			if a.Version != "" {
				results += " " + a.Version
			}
		}

		results = strings.Replace(results, "'", "", -1)
		servicesArray = append(servicesArray, results)
	}

	log.Println(fmt.Sprintf("Uploading %d found services to db...", len(hostsArray)))

	query := `UPDATE "Domains" SET services = data_table.services
				FROM (SELECT unnest($1::text[]) as name, unnest($2::text[]) as services)
				as data_table where "Domains".name = data_table.name AND ("Domains".services IS NULL OR strpos("Domains".services, data_table.services) = 0);`

	_, err = db.Exec(query, pq.Array(hostsArray), pq.Array(servicesArray))
	handleError(err)

	log.Println("Done parsing scan results")

}
