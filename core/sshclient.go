package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DecodedMsgToSSHClient 字符串信息解析为ssh客户端
func DecodedMsgToSSHClient(sshInfo string) (SSHClient, error) {
	client := NewSSHClient()
	decoded, err := base64.StdEncoding.DecodeString(sshInfo)
	if err != nil {
		return client, err
	}
	err = json.Unmarshal(decoded, &client)
	if err != nil {
		return client, err
	}
	if strings.Contains(client.IPAddress, ":") && string(client.IPAddress[0]) != "[" {
		client.IPAddress = "[" + client.IPAddress + "]"
	}
	return client, nil
}

// GenerateClient 创建ssh客户端
func (sclient *SSHClient) GenerateClient() error {
	var (
		auth         []ssh.AuthMethod
		addr         string
		clientConfig *ssh.ClientConfig
		client       *ssh.Client
		config       ssh.Config
		err          error
	)
	auth = make([]ssh.AuthMethod, 0)
	auth = append(auth, ssh.Password(sclient.Password))
	config = ssh.Config{
		Ciphers: []string{"aes128-ctr", "aes192-ctr", "aes256-ctr", "aes128-gcm@openssh.com", "arcfour256", "arcfour128", "aes128-cbc", "3des-cbc", "aes192-cbc", "aes256-cbc"},
	}
	clientConfig = &ssh.ClientConfig{
		User:    sclient.Username,
		Auth:    auth,
		Timeout: 5 * time.Second,
		Config:  config,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
	}
	addr = fmt.Sprintf("%s:%d", sclient.IPAddress, sclient.Port)
	if client, err = ssh.Dial("tcp", addr, clientConfig); err != nil {
		return err
	}
	sclient.Client = client
	return nil
}

// InitTerminal 初始化终端
func (sclient *SSHClient) InitTerminal(rows, cols int) *SSHClient {
	sshSession, err := sclient.Client.NewSession()
	if err != nil {
		log.Println(err)
		return nil
	}
	sclient.Session = sshSession
	sclient.StdinPipe, _ = sshSession.StdinPipe()
	comboWriter := new(wsBufferWriter)
	//ssh.stdout and stderr will write output into comboWriter
	sshSession.Stdout = comboWriter
	sshSession.Stderr = comboWriter
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	if err := sshSession.RequestPty("xterm", rows, cols, modes); err != nil {
		return nil
	}
	if err := sshSession.Shell(); err != nil {
		return nil
	}
	sclient.ComboOutput = comboWriter
	return sclient
}

func flushComboOutput(w *wsBufferWriter, wsConn *websocket.Conn) error {
	if w.buffer.Len() != 0 {
		bufStr := w.buffer.String()
		// 处理非utf8字符
		if !utf8.ValidString(bufStr) {
			buf := make([]rune, 0, len(bufStr))
			for _, r := range bufStr {
				if r == utf8.RuneError {
					buf = append(buf, []rune("@")...)
				} else {
					buf = append(buf, r)
				}
			}
			bufStr = string(buf)
		}
		if err := wsConn.WriteMessage(websocket.TextMessage, []byte(bufStr)); err != nil {
			return err
		}
		w.buffer.Reset()
	}
	return nil
}

// Connect ws连接
func (sclient *SSHClient) Connect(ws *websocket.Conn, timeout time.Duration) {
	stopCh := make(chan struct{})
	//这里第一个协程获取用户的输入
	go func() {
		for {
			// p为用户输入
			_, p, err := ws.ReadMessage()
			if err != nil {
				close(stopCh)
				return
			}
			if string(p) == "ping" {
				continue
			}
			if strings.Contains(string(p), "resize") {
				resizeSlice := strings.Split(string(p), ":")
				rows, _ := strconv.Atoi(resizeSlice[1])
				cols, _ := strconv.Atoi(resizeSlice[2])
				err := sclient.Session.WindowChange(rows, cols)
				if err != nil {
					log.Println(err)
					return
				}
				continue
			}
			_, err = sclient.StdinPipe.Write(p)
			if err != nil {
				close(stopCh)
				return
			}
		}
	}()

	//第二个协程将远程主机的返回结果返回给用户
	go func() {
		defer func() {
			ws.Close()
			if sclient.Session != nil {
				sclient.ComboOutput = nil
				sclient.StdinPipe.Close()
				sclient.Session.Close()
				sclient.Client.Close()
				sclient.Session = nil
				sclient.Client = nil
			}
			if err := recover(); err != nil {
				log.Println(err)
			}
		}()
		// 设置ws超时时间timer
		stopTimer := time.NewTimer(timeout)
		defer stopTimer.Stop()

		t := time.NewTicker(time.Millisecond * 20)
		defer t.Stop()

		// 主循环
		for {
			select {
			case <-stopCh:
				return
			case <-stopTimer.C:
				ws.WriteMessage(1, []byte("\033[33m已超时关闭连接!\033[0m"))
				return
			case <-t.C:
				if err := flushComboOutput(sclient.ComboOutput, ws); err != nil {
					return
				}
			}
		}
	}()

	defer func() {
		if err := recover(); err != nil {
			log.Println(err)
		}
	}()
}

// ExecRemoteCommand 执行远程命令
func (sclient *SSHClient) ExecRemoteCommand(command string) (string, error) {
	//创建ssh登陆配置
	config := &ssh.ClientConfig{
		Timeout:         time.Second, //ssh 连接time out 时间一秒钟, 如果ssh验证错误 会在一秒内返回
		User:            sclient.Username,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //这个可以， 但是不够安全
	}
	config.Auth = []ssh.AuthMethod{ssh.Password(sclient.Password)}

	//dial 获取ssh client
	addr := fmt.Sprintf("%s:%d", sclient.IPAddress, sclient.Port)
	sshClient, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		fmt.Println("创建ssh client 失败: ", err)
		return "", err
	}
	defer sshClient.Close()

	//创建ssh-session
	session, err := sshClient.NewSession()
	if err != nil {
		fmt.Println("创建ssh session 失败: ", err)
		return "", err
	}
	defer session.Close()
	//执行远程命令
	combo, err := session.CombinedOutput(command)
	if err != nil {
		fmt.Println("远程执行cmd 失败: ", err)
		return "", err
	}
	return string(combo), nil
}
