package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	k8score "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var watcher *fsnotify.Watcher
var clientset *kubernetes.Clientset
var targetPod *k8score.Pod
var config *rest.Config
var sourceDir string
var destDir string

func main() {

	src := flag.String("source", "/home/scallopboat/tempWatch", "Full path on local file system")
	dest := flag.String("dest", "/usr/share/nginx/html", "Full path on remote pod file system")
	pod := flag.String("pod", "shell-demo", "Pod name")
	namespace := flag.String("n", "default", "namespace")

	var kubeconfig *string

	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()
	fmt.Println(*src, *dest, *pod, *namespace, *kubeconfig)

	sourceDir = *src
	destDir = *dest

	var err error
	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// get pods
	targetPod, err = clientset.CoreV1().Pods(*namespace).Get(*pod, metav1.GetOptions{})

	if errors.IsNotFound(err) {
		fmt.Printf("Pod %s in namespace %s not found\n", *pod, *namespace)
	} else if statusError, isStatus := err.(*errors.StatusError); isStatus {
		fmt.Printf("Error getting pod %s in namespace %s: %v\n",
			*pod, *namespace, statusError.ErrStatus.Message)
	} else if err != nil {
		panic(err.Error())
	}

	if err != nil {
		return
	}

	// is the pod running?
	if targetPod.Status.Phase != "Running" {
		fmt.Println("Pod is not in a running state")
		return
	}

	// validate the dest dir exists
	valid := checkContainerDir(destDir)
	if !valid {
		fmt.Println("Directory doesn't exist", err)
		panic(err.Error())
	}

	// creates a new file watcher
	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	// starting at the root of the project, walk each file/directory searching for
	// directories
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		fmt.Println("Directory doesn't exist", err)
		return
	}

	// TODO Before monitoring, need to sync the entire dir structure to remote
	// validate the dest dir exists

	err = syncLocalToRemote()
	if err != nil {
		fmt.Println("Directory doesn't exist", err)
		panic(err.Error())
	}

	if err := filepath.Walk(sourceDir, watchDir); err != nil {
		fmt.Println("ERROR", err)
		return
	}

	done := make(chan bool)

	go func() {
		for {
			select {
			// watch for events
			case event := <-watcher.Events:
				log.Printf("EVENT: %s: %s", event.Op, event.Name)
				handleEvent(event)

				// watch for errors
			case err := <-watcher.Errors:
				fmt.Println("ERROR", err)
			}
		}
	}()

	<-done
}

func handleEvent(e fsnotify.Event) error {

	// Get the filename really quick, if it's not found locally, just move on...
	// avoiding temp files generated from gedit and the like

	time.Sleep(500 * time.Millisecond)

	if !fileExists(e.Name) {
		return nil
	}

	switch e.Op {
	case fsnotify.Create:
		fmt.Printf("Create %#v\n", e)
		copyToPod(e.Name)
	case fsnotify.Write:
		fmt.Printf("Write %#v\n", e)
		copyToPod(e.Name)
	case fsnotify.Remove:
		fmt.Printf("Remove %#v\n", e)
		deleteFromPod(e.Name)
	case fsnotify.Rename:
		fmt.Printf("Rename %#v\n", e)
		deleteFromPod(e.Name)
	}

	return nil
}

// watchDir gets run as a walk func, searching for directories to add watchers to
func watchDir(path string, fi os.FileInfo, err error) error {

	// since fsnotify can watch all the files in a directory, watchers only need
	// to be added to each nested directory
	if fi.Mode().IsDir() {
		return watcher.Add(path)
	}

	return nil
}

func homeDir() string {

	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func checkContainerDir(dir string) bool {

	//TODO Add param to give the option of creating the dir if it doesn't exist
	cmd := []string{"/bin/sh", "-c",
		"if [ -d \"" + dir + "\" ];\n" + `
		then
		printf 0
		else
		printf 1
		fi`}

	isDir, err := exec(cmd)

	if err != nil {
		return false
	}

	ret, err := strconv.Atoi(isDir)

	return ret == 0
}

func copyToPod(filePath string) error {

	// strip the local base dir from filepath to see if it exists remotely.
	remotePath := strings.Replace(filePath, sourceDir, destDir, -1)

	remoteDir, _ := filepath.Split(remotePath)

	valid := checkContainerDir(remoteDir)

	if !valid {
		// Create the directory
		return nil
	}

	//TODO Need to test to see if a dir was created, so it can be created
	dat, err := ioutil.ReadFile(filePath)
	filename := strings.Replace(filePath, sourceDir, "", 1)
	if err != nil {
		panic(err)
	}

	data := strings.Replace(string(dat), "\\", "\\\\", 1)

	cmd := []string{"/bin/sh", "-c",
		"printf \"" + data + "\" > '" + destDir + filename + "'"}

	_, err = exec(cmd)

	if err != nil {
		fmt.Println("ERROR: Copy failed")
		panic(err.Error())
	}

	return nil
}

func deleteFromPod(filePath string) error {

	filename := strings.Replace(filePath, sourceDir, "", 1)

	cmd := []string{"/bin/sh", "-c",
		"rm '" + destDir + filename + "'"}

	_, err := exec(cmd)

	if err != nil {
		fmt.Println("ERROR: Destination directory may be invalid")
		panic(err.Error())
	}

	return nil
}

// ExecuteRemoteCommand executes a remote shell command on the given pod
// returns the output from stdout and stderr
func exec(command []string) (string, error) {

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(targetPod.Name).
		Namespace(targetPod.Namespace).
		SubResource("exec")

	scheme := runtime.NewScheme()
	if err := k8score.AddToScheme(scheme); err != nil {
		panic(err)
	}

	parameterCodec := runtime.NewParameterCodec(scheme)
	req.VersionedParams(&k8score.PodExecOptions{
		Command:   command,
		Container: "",
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, parameterCodec)

	fmt.Println("Request URL:", req.URL().String())

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())

	if err != nil {
		panic(err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	if stderr.String() != "" {
		fmt.Println("CMD Called:", strings.Join(command, " "))
		fmt.Println("ERROR:", stderr.String())
	}

	if err != nil {
		panic(err)
	}

	return stdout.String(), nil
}

func syncLocalToRemote() error {
	// Take everything in the source dir, and copy it up to remote.
	return nil
}

// Tar takes a source and variable writers and walks 'source' writing each file
// found to the tar writer; the purpose for accepting multiple writers is to allow
// for multiple outputs (for example a file, or md5 hash)
func Tar(src string, writers ...io.Writer) error {

	// ensure the src actually exists before trying to tar it
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("Unable to tar files - %v", err.Error())
	}

	mw := io.MultiWriter(writers...)

	gzw := gzip.NewWriter(mw)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	// walk path
	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {

		// return on any error
		if err != nil {
			return err
		}

		// create a new dir/file header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		// update the name to correctly reflect the desired destination when untaring
		header.Name = strings.TrimPrefix(strings.Replace(file, src, "", -1), string(filepath.Separator))

		// write the header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// return on non-regular files (thanks to [kumo](https://medium.com/@komuw/just-like-you-did-fbdd7df829d3) for this suggested update)
		if !fi.Mode().IsRegular() {
			return nil
		}

		// open files for taring
		f, err := os.Open(file)
		if err != nil {
			return err
		}

		// copy file data into tar writer
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}

		// manually close here after each file operation; defering would cause each file close
		// to wait until all operations have completed.
		f.Close()

		return nil
	})
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
