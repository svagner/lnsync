package main

import (
	"flag"
	"github.com/howeyc/fsnotify"
	daemon "github.com/sevlyar/go-daemon"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

var source = flag.String("s", "", "Source path")
var distanation = flag.String("d", "", "Distanation path")
var signal = flag.String("signal", "", "send signal to daemon")
var pidf = flag.String("pid", "", "pid file")
var logf = flag.String("log", "", "log file")

type UpdateHeader struct {
	Event fsnotify.FileEvent
	Path  *Directory
}

type Directory struct {
	Path        string
	Update      chan UpdateHeader
	Quit        chan bool
	WatcherQuit chan bool
	Exit        chan bool
	fileWatcher *fsnotify.Watcher
}

func main() {

	handler := func(sig os.Signal) error {
		log.Println("signal:", sig)
		if sig == syscall.SIGTERM {
			os.Exit(0)
			return daemon.ErrStop
		}
		return nil
	}
	var (
		logfile,
		pidfile string
	)
	if len(*logf) == 0 {
		logfile = "/var/log/lnsync.log"
	} else {
		logfile = *logf
	}

	if len(*pidf) == 0 {
		pidfile = "/var/run/lnsync.pid"
	} else {
		pidfile = *pidf
	}

	// Define command: command-line arg, system signal and handler
	daemon.AddCommand(daemon.StringFlag(signal, "term"), syscall.SIGTERM, handler)
	daemon.AddCommand(daemon.StringFlag(signal, "reload"), syscall.SIGHUP, handler)
	flag.Parse()
	dmn := &daemon.Context{
		PidFileName: pidfile,
		PidFilePerm: 0644,
		LogFileName: logfile,
		LogFilePerm: 0640,
		WorkDir:     "/",
		Umask:       027,
	}

	if len(daemon.ActiveFlags()) > 0 {
		d, err := dmn.Search()
		if err != nil {
			log.Fatalln("Unable send signal to the daemon:", err)
		}
		daemon.SendCommands(d)
		return
	}
	child, _ := dmn.Reborn()

	if child != nil {
		return
	}
	defer dmn.Release()
	chanQuit := make(chan bool)
	chanExit := make(chan bool)
	chanWatcheQuit := make(chan bool)
	chanUpdate := make(chan UpdateHeader)
	dirs := strings.Split(*source, ",")
	if len(dirs) == 0 || len(*distanation) == 0 {
		flag.PrintDefaults()
		os.Exit(1)
	}
	manageDirs := make([]Directory, 0)
	for idx, dir := range dirs {
		manageDirs = append(manageDirs, Directory{Path: dir,
			Update:      chanUpdate,
			Quit:        chanQuit,
			WatcherQuit: chanWatcheQuit,
			Exit:        chanExit,
		})
		go manageDirs[idx].InitFSWatch()
	}

	log.Println("Starting pre-cleaner process")
	if err := cleanDirs(manageDirs, *distanation); err != nil {
		log.Fatalln("First clean dirs was corrapted: " + err.Error())
	}
	exitCnt := len(manageDirs)

	go func() {
		for {
			select {
			case _ = <-chanQuit:
				for _, dir := range manageDirs {
					dir.WatcherQuit <- true
				}
			case fileUpdate := <-chanUpdate:
				go fileUpdate.Path.UpdateDirs(*distanation, fileUpdate)
			case _ = <-chanExit:
				exitCnt--
			}
			if exitCnt == 0 {
				return
			}
		}
	}()
	err := daemon.ServeSignals()
	if err != nil {
		log.Println("Error:", err)
	}
}

func cleanDirs(sources []Directory, target string) (err error) {
	filenames := make(map[string]string)
	for _, source := range sources {
		files, err := ioutil.ReadDir(source.Path)
		if err != nil {
			return err
		}
		for _, f := range files {
			filenames[f.Name()] = source.Path
		}
	}
	files, err := ioutil.ReadDir(target)
	if err != nil {
		return err
	}
	for _, f := range files {
		info, err := os.Lstat(target + "/" + f.Name())
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == os.ModeSymlink {
			_, err := filepath.EvalSymlinks(target + "/" + f.Name())
			if err != nil {
				log.Println("Unresolved link: " + target + "/" + f.Name() + ". Deleted")
				err := os.Remove(target + "/" + f.Name())
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (d *Directory) UpdateDirs(dist string, updated UpdateHeader) error {
	if updated.Event.IsCreate() {
		err := os.Symlink(updated.Event.Name, dist+"/"+path.Base(updated.Event.Name))
		if err != nil {
			log.Println(err.Error())
			return err
		}
		log.Println("Updated link: " + updated.Event.Name)
	}
	if updated.Event.IsDelete() {
		err := os.Remove(dist + "/" + path.Base(updated.Event.Name))
		if err != nil {
			log.Println(err.Error())
			return err
		}
		log.Println("Delete link: " + dist + "/" + path.Base(updated.Event.Name))
	}

	return nil
}

func (d *Directory) InitFSWatch() {
	var err error
	d.fileWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Fatal("Filed to initialize file system watcher for <" + d.Path + ">:" + err.Error())
	}

	go d.fsEvent(d.fileWatcher)
	d.StartFSWatch()
	<-d.WatcherQuit
	d.Exit <- true
	d.fileWatcher.Close()
}

func (d *Directory) StartFSWatch() {
	err := d.fileWatcher.Watch(d.Path)
	log.Println("Add directory for watch: " + d.Path)
	if err != nil {
		log.Println("FS Monitor error monitor path [" +
			d.Path + "]: " + err.Error())
	}
}

func (d *Directory) StopFSWatch() {
	err := d.fileWatcher.RemoveWatch(d.Path)
	if err != nil {
		log.Println("Remove directory from watching [" + d.Path +
			"]: " + err.Error())
		return
	}
	log.Println("Remove directory from watching: " + d.Path)
}

func (d *Directory) fsEvent(watcher *fsnotify.Watcher) {
	for {
		select {
		case ev := <-watcher.Event:
			d.Update <- UpdateHeader{Event: *ev, Path: d}
		case err := <-watcher.Error:
			log.Println("File watcher exitting... Path: " + d.Path + ". Quit: " + err.Error())
			return
		}
	}
}
