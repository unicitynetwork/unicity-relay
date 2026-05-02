package zooid

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

var (
	instancesByHost map[string]*Instance
	instancesByName map[string]*Instance
	instancesOnce   sync.Once
	instancesMux    sync.RWMutex
)

func Dispatch(hostname string) (*Instance, bool) {
	instancesMux.RLock()
	defer instancesMux.RUnlock()

	instance, exists := instancesByHost[hostname]

	return instance, exists
}

// Start blocks until ctx is canceled. ctx is the service-level root context
// (created once in main from signal.NotifyContext) — every Instance built
// here stores it on Instance.Ctx, and every per-call DB timeout in this
// package derives from it. SIGTERM cancels ctx → in-flight DB ops abort
// instead of running their full per-op budget against a dying process.
func Start(ctx context.Context) {
	mediaDir := Env("MEDIA")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		log.Fatalf("Failed to create media directory: %v", err)
	}

	configDir := Env("CONFIG")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		log.Fatalf("Failed to create config directory: %v", err)
	}

	entries, err := os.ReadDir(configDir)
	if err != nil {
		log.Fatalf("Failed to scan config directory: %v", err)
	}

	// Build instances outside the lock so MakeInstance (DB init, cache warming)
	// doesn't block Dispatch or metrics collection.
	newByHost := make(map[string]*Instance)
	newByName := make(map[string]*Instance)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		instance, err := MakeInstance(ctx, entry.Name())

		if err != nil {
			log.Printf("Failed to make instance for %s: %v", entry.Name(), err)
		} else {
			newByHost[instance.Config.Host] = instance
			newByName[entry.Name()] = instance
			log.Printf("Loaded %s", entry.Name())
		}
	}

	instancesMux.Lock()
	instancesOnce.Do(func() {
		instancesByHost = make(map[string]*Instance)
		instancesByName = make(map[string]*Instance)
	})
	for k, v := range newByHost {
		instancesByHost[k] = v
	}
	for k, v := range newByName {
		instancesByName[k] = v
	}
	instancesMux.Unlock()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Failed to create file watcher: %v", err)
		return
	}

	defer watcher.Close()

	if err := watcher.Add(configDir); err != nil {
		log.Printf("Failed to watch config directory: %v", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			// Service shutting down — stop watching for config changes and
			// release the watcher's fd. In-flight DB ops on individual
			// instances abort via their derived contexts; that's handled
			// at each call site, not here.
			return

		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			filename := filepath.Base(event.Name)

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) {
				instancesMux.Lock()

				if instance, exists := instancesByName[filename]; exists {
					instance.Cleanup()

					delete(instancesByHost, instance.Config.Host)
					delete(instancesByName, filename)
				}

				if event.Has(fsnotify.Remove) {
					log.Printf("Unloaded %s", filename)
				} else {
					instance, err := MakeInstance(ctx, filename)
					if err != nil {
						log.Printf("Failed to reload %s: %v", filename, err)
					} else {
						instancesByHost[instance.Config.Host] = instance
						instancesByName[filename] = instance

						if event.Has(fsnotify.Write) {
							log.Printf("Reloaded %v", filename)
						} else {
							log.Printf("Loaded %v", filename)
						}
					}
				}

				instancesMux.Unlock()
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}

			log.Printf("File watcher error: %v", err)
		}
	}
}
