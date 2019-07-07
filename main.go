package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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

//
var watcher *fsnotify.Watcher
var clientset *kubernetes.Clientset
var targetPod *k8score.Pod
var config *rest.Config

// main
func main() {

	sourceDir := flag.String("source", "/home/scallopboat/tempWatch", "Full path on local file system")
	destDir := flag.String("dest", "/tmp", "Full path on remote pod file system")
	pod := flag.String("pod", "example-memcached-c88c4dc9f-r5v8l", "Pod name")
	namespace := flag.String("n", "default", "namespace")

	var kubeconfig *string

	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	flag.Parse()

	fmt.Println(*sourceDir, *destDir, *pod, *namespace, *kubeconfig)

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
	checkContainerDir(*destDir)
	//TODO Validate pod name, namespace, destDir

	// creates a new file watcher
	watcher, _ = fsnotify.NewWatcher()
	defer watcher.Close()

	// starting at the root of the project, walk each file/directory searching for
	// directories
	if _, err := os.Stat(*sourceDir); os.IsNotExist(err) {
		fmt.Println("Directory doesn't exist", err)
		return
	}

	// TODO Before monitoring, need to sync the entire dir structure to remote

	if err := filepath.Walk(*sourceDir, watchDir); err != nil {
		fmt.Println("ERROR", err)
		return
	}

	done := make(chan bool)

	go func() {
		for {
			select {
			// watch for events
			case event := <-watcher.Events:
				fmt.Printf("EVENT! %#v\n", event)
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
	switch e.Op {
	case fsnotify.Create:
		fmt.Printf("Create %#v\n", e)
	case fsnotify.Write:
		fmt.Printf("Write %#v\n", e)
	case fsnotify.Remove:
		fmt.Printf("Remove %#v\n", e)
	case fsnotify.Rename:
		fmt.Printf("Rename %#v\n", e)
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

func checkContainerDir(dir string) error {

	//TODO Add param to give the option of creating the dir if it doesn't exist
	cmd := []string{"/bin/sh", "-c",
		"if [ -d \"" + dir + "\" ];\n" + `
		then
		return 0
		else
		return 1
		fi`}

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
	if err != nil {
		panic(err)
	}

	return stdout.String(), nil
}

/*
func checkContainerDir(config string, destDir string, targetPod *k8score.Pod) error {

	config := &restclient.Config{
		Host: "http://192.168.8.175:8080",
		Insecure: true,
	}

	config.ContentConfig.GroupVersion = &api.Unversioned
	config.ContentConfig.NegotiatedSerializer = api.Codecs

	restClient, err := restclient.RESTClientFor(config)
	if err != nil {
		panic(err.Error())
	}

	req := restClient.Post().Resource("pods").Name("wordpress-mysql-213049546-29s7d").Namespace("default").SubResource("exec").Param("container", "mysql")
	req.VersionedParams(&api.PodExecOptions{
		Container: "mysql",
		Command:   []string{"ls"},
		Stdin:     true,
		Stdout:    true,
	}, api.ParameterCodec)

	exec, err := remotecommand.NewExecutor(config, "POST", req.URL())

		if err != nil {
			panic(err.Error())
		}

	sopt := remotecommand.StreamOptions{
		SupportedProtocols: remotecommandserver.SupportedStreamingProtocols,
		Stdin:              os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Tty:    false,
	}

	err = exec.Stream(sopt)
	if err != nil {
		panic(err.Error())
	}

	return nil
}

func copyToContainer(config string, srcFile string, destFile string, targetPod k8score.Pod) error {
	cfg := kube.Config{
		Kubeconfig: config,
		Image:      targetPod.Spec.Containers[0].Image,
		Name:       targetPod.ObjectMeta.Name,
		Namespace:  targetPod.ObjectMeta.Namespace,
	}

	// also sleeping for a couple of seconds
	// if the pod completes too fast, we don't have time to attach to it

	cmd := kube.Command(cfg, "/bin/sh", "-c", "cat", srcFile, ">", destFile)
	cmd.Stdout = os.Stdout

	err := cmd.Run()
	if err != nil {
		return err
	}

	return nil
}
*/
