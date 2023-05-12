package main

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"
)

//go:embed frontend/css/* frontend/js/*
var AssetFS embed.FS

var ug = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024 * 10,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func main() {
	var router = gin.Default().Delims("[[", "]]")

	router.LoadHTMLGlob("frontend/*.gohtml")
	frontend, _ := fs.Sub(AssetFS, "frontend")
	router.StaticFS("frontend", http.FS(frontend))

	router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "root.gohtml", gin.H{})
	})

	router.GET("/frame", func(c *gin.Context) {
		c.HTML(http.StatusOK, "frame.gohtml", gin.H{})
	})

	router.GET("/api", SSH)

	Server := http.Server{Addr: "0.0.0.0:80", Handler: router}

	//region Shutdown Server
	go func() {
		if err := Server.ListenAndServe(); err != nil && errors.Is(err, http.ErrServerClosed) {
			log.Printf("listen: %s\n", err)
		}
	}()

	exit := make(chan os.Signal)
	signal.Notify(exit, syscall.SIGTERM, syscall.SIGINT) // kill & kill -2
	<-exit
	log.Println("Shutting down server...")

	c, f := context.WithTimeout(context.Background(), 5*time.Second)
	defer f()

	if err := Server.Shutdown(c); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
	//endregion
}

func SSH(c *gin.Context) {
	hostname := c.Query("hostname")
	username := c.Query("username")
	password := c.Query("password")

	// 连接ssh
	sshClient, err := ssh.Dial("tcp", hostname, &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{ssh.Password(password)},

		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		log.Println(err.Error())
	}
	defer sshClient.Close()

	// 新建会话
	session, err := sshClient.NewSession()
	if err != nil {
		log.Println(err)
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	// 定义pty请求消息体,请求标准输出的终端
	err = session.RequestPty("xterm", 24, 100, modes)
	if err != nil {
		log.Println(err.Error())
	}

	conn, err := ug.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()

	// session开始start处理shell
	err = session.Shell()
	if err != nil {
		log.Println(err.Error())
	}

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				log.Println(err.Error())
				return
			}

			_, err = stdin.Write(msg)
			if err != nil && err.Error() != io.EOF.Error() {
				log.Println(err.Error())
				return
			}
		}
	}()

	br := bufio.NewReader(stdout)
	var buf []byte
	t := time.NewTimer(time.Microsecond * 100)
	defer t.Stop()

	r := make(chan rune)

	// 接收ssh并转发给ws
	go func() {
		for {
			x, size, err := br.ReadRune()
			if err != nil {
				log.Println(err.Error())
				return
			}
			if size > 0 {
				r <- x
			}
		}
	}()

	for {
		select {
		case d := <-r:
			if d != utf8.RuneError {
				p := make([]byte, utf8.RuneLen(d))
				utf8.EncodeRune(p, d)
				buf = append(buf, p...)
			} else {
				buf = append(buf, []byte("@")...)
			}
		case <-t.C:
			if len(buf) != 0 {
				fmt.Println(buf)
				err := conn.WriteMessage(websocket.TextMessage, buf)
				if err != nil {
					return
				}
				buf = []byte{}
			}
			t.Reset(time.Microsecond * 100)
		}
	}
}
