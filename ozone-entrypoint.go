package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	ozoneHome = "/ozone"
	javaHome  = "/usr"
)

var commands = map[string]string{
	"datanode": "org.apache.hadoop.ozone.HddsDatanodeService",
	"om":       "org.apache.hadoop.ozone.om.OzoneManagerStarter",
	"scm":      "org.apache.hadoop.hdds.scm.server.StorageContainerManagerStarter",
	"s3g":      "org.apache.hadoop.ozone.s3.Gateway",
	"recon":    "org.apache.hadoop.ozone.recon.ReconServer",
}

var artifactNames = map[string]string{
	"datanode": "ozone-datanode",
	"om":       "ozone-manager",
	"scm":      "hdds-server-scm",
	"s3g":      "ozone-s3gateway",
	"recon":    "ozone-recon",
}

// buildClasspath собирает classpath для конкретной команды Ozone.
// Читает только соответствующий .classpath файл (как оригинальный shell-скрипт через OZONE_RUN_ARTIFACT_NAME),
// раскрывает переменную $HDDS_LIB_JARS_DIR и отрезает префикс "classpath=".
// Glob lib/*.jar добавляет любые JAR-ы, не попавшие в .classpath файл.
func buildClasspath(command string) string {
	libDir := ozoneHome + "/share/ozone/lib"

	seen := make(map[string]struct{})
	var entries []string

	add := func(path string) {
		if _, ok := seen[path]; !ok {
			seen[path] = struct{}{}
			entries = append(entries, path)
		}
	}

	// Директория конфигов должна быть первой в classpath
	confDir := ozoneHome + "/etc/hadoop"
	if info, err := os.Stat(confDir); err == nil && info.IsDir() {
		add(confDir)
	}

	// Читаем только .classpath файл для данной команды.
	// Формат файла: classpath=$HDDS_LIB_JARS_DIR/a.jar:$HDDS_LIB_JARS_DIR/b.jar:...
	// $HDDS_LIB_JARS_DIR раскрываем в реальный путь к lib/.
	if artifact, ok := artifactNames[command]; ok {
		cpFile := ozoneHome + "/share/ozone/classpath/" + artifact + ".classpath"
		if f, err := os.Open(cpFile); err == nil {
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				// Убираем префикс "classpath=" если есть
				line = strings.TrimPrefix(line, "classpath=")
				for _, part := range strings.Split(line, ":") {
					part = strings.TrimSpace(part)
					// Раскрываем $HDDS_LIB_JARS_DIR
					part = strings.ReplaceAll(part, "$HDDS_LIB_JARS_DIR", libDir)
					if part != "" {
						add(part)
					}
				}
			}
			f.Close()
		}
	}

	// Добавляем опциональные JAR-ы из lib/<artifact>/ (аналог OPTIONAL_CLASSPATH_DIR в shell).
	// Например, lib/ozone-manager/ содержит Ranger plugin JARs.
	// Намеренно НЕ делаем glob lib/*.jar — он подтягивал бы лишние JARы (например jersey-server 1.x),
	// которых нет в .classpath файле и которые ломают CDI-инициализацию Weld в s3g.
	if artifact, ok := artifactNames[command]; ok {
		optJars, _ := filepath.Glob(libDir + "/" + artifact + "/*.jar")
		for _, j := range optJars {
			add(j)
		}
	}

	return strings.Join(entries, ":")
}

// buildJvmArgs формирует полный список аргументов для запуска java-процесса.
func buildJvmArgs(mainClass, classpath string, extraArgs []string) []string {
	javaBin := javaHome + "/bin/java"
	args := []string{javaBin}

	for _, envVar := range []string{"OZONE_OPTS", "JAVA_OPTS"} {
		if opts := os.Getenv(envVar); opts != "" {
			args = append(args, strings.Fields(opts)...)
		}
	}

	nativeLibPath := strings.Join([]string{
		ozoneHome + "/share/ozone/lib/native",
		"/usr/lib64",
	}, ":")

	args = append(args,
		"-Djava.library.path="+nativeLibPath,
		"-Dozone.home="+ozoneHome,
		"-Dhadoop.home.dir="+ozoneHome,
		"-cp", classpath,
		mainClass,
	)
	return append(args, extraArgs...)
}

// prepareEnv добавляет необходимые переменные окружения, если они ещё не установлены.
func prepareEnv() []string {
	env := os.Environ()
	hasVar := func(key string) bool {
		prefix := key + "="
		for _, e := range env {
			if strings.HasPrefix(e, prefix) {
				return true
			}
		}
		return false
	}
	if !hasVar("OZONE_HOME") {
		env = append(env, "OZONE_HOME="+ozoneHome)
	}
	if !hasVar("JAVA_HOME") {
		env = append(env, "JAVA_HOME="+javaHome)
	}
	return env
}

// runJava запускает java-процесс, ждёт завершения и возвращает ошибку.
// Используется для шагов инициализации (--init, --bootstrap).
func runJava(mainClass, classpath string, args []string, env []string) error {
	jvmArgs := buildJvmArgs(mainClass, classpath, args)
	cmd := exec.Command(jvmArgs[0], jvmArgs[1:]...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// execJava заменяет текущий процесс через syscall.Exec — PID 1 становится java.
func execJava(mainClass, classpath string, args []string, env []string) {
	javaBin := javaHome + "/bin/java"
	jvmArgs := buildJvmArgs(mainClass, classpath, args)
	if err := syscall.Exec(javaBin, jvmArgs, env); err != nil {
		fmt.Fprintf(os.Stderr, "exec %s: %v\n", javaBin, err)
		os.Exit(1)
	}
}

func logf(format string, a ...interface{}) {
	fmt.Printf("[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// waitFor опрашивает TCP-адрес addr (host:port) каждые interval до таймаута.
// Используется в init-контейнерах вместо "while ! nc -z host port; do sleep 1; done".
func waitFor(addr string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	logf("Waiting for %s (timeout: %s, interval: %s)", addr, timeout, interval)
	for {
		conn, err := net.DialTimeout("tcp", addr, interval)
		if err == nil {
			conn.Close()
			logf("%s is available", addr)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s: %v", addr, err)
		}
		time.Sleep(interval)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: ozone <command> [args...]")
		fmt.Fprintln(os.Stderr, "Commands: datanode, om, scm, s3g, recon, scm-start, om-start, idle")
		fmt.Fprintln(os.Stderr, "          wait-for <host:port> [--timeout=120s] [--interval=2s]")
		os.Exit(1)
	}

	command := os.Args[1]
	env := prepareEnv()

	switch command {

	case "idle":
		select {}

	// wait-for <host:port> [--timeout=120s] [--interval=2s]
	// Заменяет "while ! nc -z host port; do sleep 1; done" в init-контейнерах.
	// Пример: ozone wait-for scm:9894 --timeout=180s
	case "wait-for":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: ozone wait-for <host:port> [--timeout=120s] [--interval=2s]")
			os.Exit(1)
		}
		addr := os.Args[2]
		timeout := 120 * time.Second
		interval := 2 * time.Second
		for _, arg := range os.Args[3:] {
			var val string
			if strings.HasPrefix(arg, "--timeout=") {
				val = strings.TrimPrefix(arg, "--timeout=")
				if d, err := time.ParseDuration(val); err == nil {
					timeout = d
				}
			} else if strings.HasPrefix(arg, "--interval=") {
				val = strings.TrimPrefix(arg, "--interval=")
				if d, err := time.ParseDuration(val); err == nil {
					interval = d
				}
			}
		}
		if err := waitFor(addr, timeout, interval); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	// scm-start — полная последовательность запуска SCM без shell:
	//   1. Только на примордиальном узле (POD_NAME оканчивается на "-0-0"):
	//      если VERSION-файл отсутствует — выполнить ozone scm --init
	//   2. Всегда — ozone scm --bootstrap
	//   3. exec ozone scm
	case "scm-start":
		classpath := buildClasspath("scm")
		podName := os.Getenv("POD_NAME")
		if strings.HasSuffix(podName, "-0-0") {
			versionFile := "/data/metadata/scm/current/VERSION"
			if _, err := os.Stat(versionFile); os.IsNotExist(err) {
				logf("Initializing SCM cluster...")
				if err := runJava(commands["scm"], classpath, []string{"--init"}, env); err != nil {
					fmt.Fprintf(os.Stderr, "scm --init failed: %v\n", err)
					os.Exit(1)
				}
			} else {
				logf("SCM already initialized, skipping init")
			}
		}
		logf("Bootstrapping SCM...")
		if err := runJava(commands["scm"], classpath, []string{"--bootstrap"}, env); err != nil {
			fmt.Fprintf(os.Stderr, "scm --bootstrap failed: %v\n", err)
			os.Exit(1)
		}
		execJava(commands["scm"], classpath, []string{}, env)

	// om-start — полная последовательность запуска OM без shell:
	//   1. Если VERSION-файл отсутствует — выполнить ozone om --init
	//   2. exec ozone om
	case "om-start":
		classpath := buildClasspath("om")
		versionFile := "/data/metadata/om/current/VERSION"
		if _, err := os.Stat(versionFile); os.IsNotExist(err) {
			logf("Initializing OM cluster...")
			if err := runJava(commands["om"], classpath, []string{"--init"}, env); err != nil {
				fmt.Fprintf(os.Stderr, "om --init failed: %v\n", err)
				os.Exit(1)
			}
		} else {
			logf("OM already initialized, skipping init")
		}
		execJava(commands["om"], classpath, []string{}, env)

	// Стандартные команды: datanode, om, scm, s3g, recon (+ произвольные флаги)
	default:
		mainClass, ok := commands[command]
		if !ok {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
			fmt.Fprintln(os.Stderr, "Available commands: datanode, om, scm, s3g, recon, scm-start, om-start, idle, wait-for")
			os.Exit(1)
		}
		classpath := buildClasspath(command)
		execJava(mainClass, classpath, os.Args[2:], env)
	}
}
