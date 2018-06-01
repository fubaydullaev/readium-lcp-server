/*
 * Copyright (c) 2016-2018 Readium Foundation
 *
 * Redistribution and use in source and binary forms, with or without modification,
 * are permitted provided that the following conditions are met:
 *
 *  1. Redistributions of source code must retain the above copyright notice, this
 *     list of conditions and the following disclaimer.
 *  2. Redistributions in binary form must reproduce the above copyright notice,
 *     this list of conditions and the following disclaimer in the documentation and/or
 *     other materials provided with the distribution.
 *  3. Neither the name of the organization nor the names of its contributors may be
 *     used to endorse or promote products derived from this software without specific
 *     prior written permission
 *
 *  THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
 *  ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
 *  WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
 *  DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE LIABLE FOR
 *  ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
 *  (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
 *  LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
 *  ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 *  (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
 *  SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

package main

import (
	"context"
	"crypto/tls"
	"os"
	"time"

	"github.com/gorilla/mux"
	"github.com/readium/readium-lcp-server/controller/lcpserver"
	"github.com/readium/readium-lcp-server/lib/filestor"
	"github.com/readium/readium-lcp-server/lib/http"
	"github.com/readium/readium-lcp-server/lib/logger"
	"github.com/readium/readium-lcp-server/lib/pack"
	"github.com/readium/readium-lcp-server/model"
	goHttp "net/http"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
)

func main() {
	logz := logger.New()

	var storagePath, certFile, privKeyFile string
	var err error
	logz.Printf("RUNNING LCP SERVER")
	configFile := "config.yaml"
	if os.Getenv("READIUM_LCPSERVER_CONFIG") != "" {
		configFile = os.Getenv("READIUM_LCPSERVER_CONFIG")
	}

	logz.Printf("Reading config " + configFile)
	cfg, err := http.ReadConfig(configFile)
	if err != nil {
		panic(err)
	}
	if certFile = cfg.Certificate.Cert; certFile == "" {
		panic("Must specify a certificate")
	}
	if privKeyFile = cfg.Certificate.PrivateKey; privKeyFile == "" {
		panic("Must specify a private key")
	}

	authFile := cfg.LcpServer.AuthFile
	if authFile == "" {
		panic("Must have passwords file")
	}
	cert, err := tls.LoadX509KeyPair(certFile, privKeyFile)
	if err != nil {
		panic(err)
	}

	// use a sqlite db by default
	dbURI := "sqlite3://file:lcp.sqlite?cache=shared&mode=rwc"
	if cfg.LcpServer.Database != "" {
		dbURI = cfg.LcpServer.Database
	}

	// Init database
	stor, err := model.SetupDB(dbURI, logz, false)
	if err != nil {
		panic("Error setting up the database : " + err.Error())
	}
	err = stor.AutomigrateForLCP()
	if err != nil {
		panic("Error migrating database : " + err.Error())
	}

	if storagePath = cfg.Storage.FileSystem.Directory; storagePath == "" {
		storagePath = "files"
	}

	var s3Storage filestor.Store
	if mode := cfg.Storage.Mode; mode == "s3" {
		s3Conf := filestor.S3Config{
			ID:             cfg.Storage.AccessId,
			Secret:         cfg.Storage.Secret,
			Token:          cfg.Storage.Token,
			Endpoint:       cfg.Storage.Endpoint,
			Bucket:         cfg.Storage.Bucket,
			Region:         cfg.Storage.Region,
			DisableSSL:     cfg.Storage.DisableSSL,
			ForcePathStyle: cfg.Storage.PathStyle,
		}
		s3Storage, _ = filestor.S3(s3Conf)
	} else {
		os.MkdirAll(storagePath, os.ModePerm) //ignore the error, the folder can already exist
		s3Storage = filestor.NewFileSystem(storagePath, cfg.LcpServer.PublicBaseUrl+"/files")
	}
	// Prepare packager with S3 and storage
	packager := pack.NewPackager(s3Storage, stor.Content(), 4)
	_, err = os.Stat(authFile)
	if err != nil {
		panic(err)
	}
	//
	// starting server

	// writing static
	static := cfg.LcpServer.Directory
	if static == "" {
		_, file, _, _ := runtime.Caller(0)
		here := filepath.Dir(file)
		static = filepath.Join(here, "../../web/lcp")
	}
	filepathConfigJs := filepath.Join(static, "/config.js")
	fileConfigJs, err := os.Create(filepathConfigJs)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := fileConfigJs.Close(); err != nil {
			panic(err)
		}
	}()

	configJs := "// This file is automatically generated, and git-ignored.\n// To ignore your local changes, use:\n// git update-index --assume-unchanged lcpserver/manage/config.js\n\nvar Config = {\n    lcp: {url: '" + cfg.LcpServer.PublicBaseUrl + "', user:'" + cfg.LcpUpdateAuth.Username + "', password: '" + cfg.LcpUpdateAuth.Password + "'},\n    lsd: {url: '" + cfg.LsdServer.PublicBaseUrl + "', user:'" + cfg.LcpUpdateAuth.Username + "', password: '" + cfg.LcpUpdateAuth.Password + "'}\n}\n"

	logz.Printf("manage/index.html config.js:")
	logz.Printf(configJs)
	fileConfigJs.WriteString(configJs)

	muxer := mux.NewRouter()

	muxer.Use(
		http.RecoveryHandler(http.RecoveryLogger(logz), http.PrintRecoveryStack(true)),
		http.CorsMiddleWare(
			http.AllowedOrigins([]string{"*"}),
			http.AllowedMethods([]string{"PATCH", "HEAD", "POST", "GET", "OPTIONS", "PUT", "DELETE"}),
			http.AllowedHeaders([]string{"Range", "Content-Type", "Origin", "X-Requested-With", "Accept", "Accept-Language", "Content-Language", "Authorization"}),
		),
		http.DelayMiddleware,
	)

	if static != "" {
		//logz.Infof("Serving static from %q", static)
		muxer.PathPrefix("/static/").Handler(goHttp.StripPrefix("/static/", goHttp.FileServer(goHttp.Dir(static))))
	}

	server := &http.Server{
		Server: goHttp.Server{
			Handler:        muxer,
			Addr:           ":" + strconv.Itoa(cfg.LcpServer.Port),
			WriteTimeout:   15 * time.Second,
			ReadTimeout:    15 * time.Second,
			MaxHeaderBytes: 1 << 20,
		},
		Log:      logz,
		Cfg:      cfg,
		Readonly: cfg.LcpServer.ReadOnly,
		St:       &s3Storage,
		Model:    stor,
		Cert:     &cert,
		Src:      pack.ManualSource{},
	}

	server.InitAuth("Readium License Content Protection Server", cfg.LcpServer.AuthFile) // creates authority checker

	// CreateDefaultLinks inits the global var DefaultLinks from config data
	// ... DefaultLinks used in several places.
	model.DefaultLinks = make(map[string]string)
	for key := range cfg.License.Links {
		model.DefaultLinks[key] = cfg.License.Links[key]
	}

	logz.Printf("License server running on port %d [Readonly %t]", cfg.LcpServer.Port, cfg.LcpServer.ReadOnly)
	// Route.PathPrefix: http://www.gorillatoolkit.org/pkg/mux#Route.PathPrefix
	// Route.Subrouter: http://www.gorillatoolkit.org/pkg/mux#Route.Subrouter
	// Router.StrictSlash: http://www.gorillatoolkit.org/pkg/mux#Router.StrictSlash
	lcpserver.RegisterRoutes(muxer, server)

	server.Src.Feed(packager.Incoming)

	logz.Printf("Using database " + dbURI)
	logz.Printf("Public base URL=" + cfg.LcpServer.PublicBaseUrl)
	logz.Printf("License links:")
	for nameOfLink, link := range cfg.License.Links {
		logz.Printf("  " + nameOfLink + " => " + link)
	}

	// Run our server in a goroutine so that it doesn't block.
	go func() {
		if err := server.ListenAndServe(); err != nil {
			logz.Printf("Error " + err.Error())
		}
	}()

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c

	wait := time.Second * 15 // the duration for which the server gracefully wait for existing connections to finish
	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	server.Shutdown(ctx)
	// Optionally, you could run srv.Shutdown in a goroutine and block on
	// <-ctx.Done() if your application should wait for other services
	// to finalize based on context cancellation.
	logz.Printf("server is shutting down.")
	os.Exit(0)
}
