package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

/* ------------------------------------------------------------------
   CONFIGURATION DEFAULTS (override with flags at runtime)
   ------------------------------------------------------------------*/

const (
	defaultAPIURL  = "https://192.168.0.1"	//Change to your Scale HC3 node IP
	defaultAPIUser = "admin" 				//Change to your Scale HC3 node username with access. Default admin
	defaultAPIPass = "admin"				//Change to your Scale HC3 user password. Default admin

	defaultSharePrefix = "smb://WORKGROUP;sambauser:somepassword@192.168.0.1/sambashare/" // Your SMB URI

	defaultOVADir = "/data/vms/ova" 		//Where your OVA files are exported to
	defaultScaleDir = "/data/vms/scale"		// Where you will export and import dummy VMs from Scale HC3 to/from
)

/* ------------------------------------------------------------------
   COMMAND-LINE FLAGS
   ------------------------------------------------------------------*/

// selection / behaviour
var (
	vmsFlag = flag.String("vms", "", "Comma-separated VM names (skip menu)")
	dryRun  = flag.Bool("n", false, "Dry-run – print, no writes")
	autoImp = flag.Bool("import", false, "Auto-import without prompt")
)

// external system
var (
	apiURL  = flag.String("api", defaultAPIURL, "Scale REST base URL")
	apiUser = flag.String("user", defaultAPIUser, "API username")
	apiPass = flag.String("pass", defaultAPIPass, "API password")
	share   = flag.String("share", defaultSharePrefix, "SMB share prefix")
)

/*--------- main ---------*/

func main() {
	flag.Parse()

	candidates, err := discoverVMs()
	must(err, "discovering VMs")
	if len(candidates) == 0 {
		log.Fatalf("no valid VM dirs beneath ", *share )
	}

	var vms []string
	if *vmsFlag != "" {
		vms = strings.Split(*vmsFlag, ",")
	} else {
		vms, err = promptUser(candidates)
		must(err, "parsing selection")
	}
	if len(vms) == 0 {
		log.Println("nothing selected – exiting")
		return
	}

	for _, vm := range vms {
		vm = strings.TrimSpace(vm)
		if err := processVM(vm); err != nil {
			log.Printf("❌ %s: %v", vm, err)
		}
	}
}

/*--------- discovery & prompt ---------*/

func discoverVMs() ([]string, error) {
	root := defaultOVADir
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		vm := e.Name()
		if len(mustGlob(filepath.Join(root, vm, "*.ovf"))) == 0 {
			continue
		}
		if fileExists(filepath.Join(defaultScaleDir, vm, vm+".xml")) {
			out = append(out, vm)
		}
	}
	sort.Strings(out)
	return out, nil
}

func promptUser(opts []string) ([]string, error) {
	fmt.Println("Select VM(s) to update:")
	for i, vm := range opts {
		fmt.Printf("  %2d) %s\n", i+1, vm)
	}
	fmt.Print("Enter number(s) separated by comma (or 'all'): ")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "all") {
		return opts, nil
	}

	var sel []string
	for _, tok := range strings.Split(line, ",") {
		i, err := strconv.Atoi(strings.TrimSpace(tok))
		if err != nil || i < 1 || i > len(opts) {
			return nil, fmt.Errorf("invalid selection %q", tok)
		}
		sel = append(sel, opts[i-1])
	}
	return sel, nil
}

/*--------- per-VM workflow ---------*/

func processVM(vm string) error {
	fmt.Printf("\n=== %s ===\n", vm)
	scaleDir := filepath.Join(defaultScaleDir, vm)

	// 1. delete existing qcow2 images
	if err := deleteQcow2(scaleDir); err != nil {
		return err
	}

	// 2. copy VMDKs → qcow2
	ovfPath := mustGlob(filepath.Join(defaultOVADir, vm, "*.ovf"))[0]
	srcFiles, err := diskFilesFromOVF(ovfPath)
	if err != nil {
		return err
	}
	dstUUIDs, err := uuidsFromScaleXML(filepath.Join(scaleDir, vm+".xml"))
	if err != nil {
		return err
	}

	if len(srcFiles) != len(dstUUIDs) {
		fmt.Printf("⚠️  mismatch: %d OVF vs %d Scale – pairing minimum\n", len(srcFiles), len(dstUUIDs))
	}
	for i := 0; i < min(len(srcFiles), len(dstUUIDs)); i++ {
		src := filepath.Join(defaultOVADir, vm, srcFiles[i])
		dst := filepath.Join(scaleDir, dstUUIDs[i]+".qcow2")
		if *dryRun {
			fmt.Printf("[dry-run] copy %s → %s\n", filepath.Base(src), filepath.Base(dst))
		} else {
			if err := copyFile(src, dst); err != nil {
				return err
			}
			fmt.Printf("✓ %s → %s\n", filepath.Base(src), filepath.Base(dst))
		}
	}

	// 3. rewrite tags block in Scale XML
	xmlPath := filepath.Join(scaleDir, vm+".xml")
	if err := rewriteTags(xmlPath); err != nil {
		return fmt.Errorf("update tags: %w", err)
	}

	// 4. optional import via REST
	if *dryRun {
		return nil
	}
	proceed := *autoImp
	if !*autoImp {
		fmt.Print("Import VM via API? (y/N): ")
		resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		proceed = strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp)), "y")
	}
	if proceed {
		if err := importVM(vm); err != nil {
			return err
		}
	}
	return nil
}

/*--------- step 1 – delete qcow2 ---------*/

func deleteQcow2(dir string) error {
	for _, p := range mustGlob(filepath.Join(dir, "*.qcow2")) {
		if *dryRun {
			fmt.Printf("[dry-run] delete %s\n", filepath.Base(p))
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		fmt.Printf("🗑 removed %s\n", filepath.Base(p))
	}
	return nil
}

/*--------- step 3 – tag rewrite ---------*/

func rewriteTags(path string) error {
	in, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// remove any existing <tags> … </tags>
	reTags := regexp.MustCompile(`(?s)<tags[^>]*>.*?</tags>`)
	out := reTags.ReplaceAll(in, []byte(""))

	// insert new tags before </scale-metadata>
	reClose := regexp.MustCompile(`</scale-metadata>`)
	if !reClose.Match(out) {
		return fmt.Errorf("no </scale-metadata> found")
	}
	insert := "      <tags>\n        <tag name=\"imported_by_script\"/>\n      </tags>\n    </scale-metadata>"
	out = reClose.ReplaceAll(out, []byte(insert))

	if *dryRun {
		fmt.Printf("[dry-run] would update tags in %s\n", filepath.Base(path))
		return nil
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

/*--------- import API ---------*/

func importVM(vm string) error {
	target := strings.TrimRight(*apiURL, "/") + "/rest/v1/VirDomain/import"

	reqBody := map[string]any{
		"source": map[string]any{
			"pathURI":                 *share + vm,   // ← use *share
			"format":                  "qcow2",
			"definitionFileName":      vm + ".xml",
			"allowNonSequentialWrites": true,
			"parallelCountPerTransfer": 0,
		},
	}
	j, _ := json.Marshal(reqBody)

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Timeout: 60 * time.Second, Transport: tr}

	req, _ := http.NewRequest("POST", target, strings.NewReader(string(j)))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if *apiUser != "" {
		req.SetBasicAuth(*apiUser, *apiPass)
	}

	fmt.Printf("⟳ Importing %s…\n", vm)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		TaskTag     string `json:"taskTag"`
		CreatedUUID string `json:"createdUUID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}

	fmt.Printf("✅ import queued: task %s (UUID %s)\n", out.TaskTag, out.CreatedUUID)
	return nil
}

/*--------- OVF helpers ---------*/

func diskFilesFromOVF(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type entry struct{ id, href string }
	var files []entry
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "File" {
			var id, href string
			for _, a := range se.Attr {
				if a.Name.Local == "id" {
					id = a.Value
				} else if a.Name.Local == "href" {
					href = a.Value
				}
			}
			files = append(files, entry{id, href})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		re := regexp.MustCompile(`file(\d+)`)
		li, lj := re.FindStringSubmatch(files[i].id), re.FindStringSubmatch(files[j].id)
		if len(li) == 2 && len(lj) == 2 {
			return li[1] < lj[1]
		}
		return files[i].id < files[j].id
	})
	out := make([]string, len(files))
	for i, fe := range files {
		out[i] = fe.href
	}
	return out, nil
}

/*--------- Scale XML helpers ---------*/

func uuidsFromScaleXML(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := xml.NewDecoder(newNullStripper(f))
	var uuids []string
	inDisk := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if se.Name.Local == "disk" {
				diskType, device := "", ""
				for _, a := range se.Attr {
					if a.Name.Local == "type" {
						diskType = a.Value
					} else if a.Name.Local == "device" {
						device = a.Value
					}
				}
				inDisk = diskType == "network" && device == "disk"
			} else if inDisk && se.Name.Local == "source" {
				for _, a := range se.Attr {
					if a.Name.Local == "name" {
						parts := strings.Split(a.Value, "/")
						uuids = append(uuids, parts[len(parts)-1])
					}
				}
			}
		case xml.EndElement:
			if se.Name.Local == "disk" {
				inDisk = false
			}
		}
	}
	return uuids, nil
}

/*--------- strip NULs ---------*/

type nullStripper struct{ r io.Reader }

func newNullStripper(r io.Reader) io.Reader { return nullStripper{r} }

func (n nullStripper) Read(p []byte) (int, error) {
	nRead, err := n.r.Read(p)
	if nRead > 0 {
		dst := p[:0]
		for _, b := range p[:nRead] {
			if b != 0x00 {
				dst = append(dst, b)
			}
		}
		nRead = len(dst)
	}
	return nRead, err
}

/*--------- misc helpers ---------*/

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func must(err error, ctx string) {
	if err != nil {
		log.Fatalf("%s: %v", ctx, err)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func mustGlob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	return m
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
