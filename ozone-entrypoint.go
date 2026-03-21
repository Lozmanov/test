package main

import (
	"bufio"
	"fmt"
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

// Маппинг команд на Java main-классы Apache Ozone 1.4.x
var commands = map[string]string{
	"datanode": "org.apache.hadoop.ozone.HddsDatanodeService",
	"om":       "org.apache.hadoop.ozone.om.OzoneManagerStarter",
	"scm":      "org.apache.hadoop.hdds.scm.server.StorageContainerManagerStarter",
	"s3g":      "org.apache.hadoop.ozone.s3.Gateway",
	"recon":    "org.apache.hadoop.ozone.recon.ReconServer",
}

// buildClasspath собирает classpath из JAR-файлов и .classpath-файлов дистрибутива.
// Директория конфигов ставится первой — Hadoop загружает ozone-site.xml через ClassLoader.
// Порядок приоритетов: conf dir → .classpath файлы (build-time порядок) → lib/*.jar fallback.
// .classpath файлы генерируются при сборке Ozone и содержат единственно верный порядок JAR-ов.
// Glob lib/*.jar используется только если .classpath файлы отсутствуют.
func buildClasspath() string {
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

	// Разбираем .classpath-файлы (формат: одна запись на строку или через двоеточие).
	// Этот порядок воспроизводит оригинальный ozone-shell-скрипт.
	cpFiles, _ := filepath.Glob(ozoneHome + "/share/ozone/classpath/*.classpath")
	for _, cpFile := range cpFiles {
		f, err := os.Open(cpFile)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			for _, part := range strings.Split(line, ":") {
				part = strings.TrimSpace(part)
				if part != "" {
					add(part)
				}
			}
		}
		f.Close()
	}

	// Fallback: если .classpath файлы отсутствуют — берём все JAR из lib/ напрямую.
	if len(entries) <= 1 {
		jars, _ := filepath.Glob(ozoneHome + "/share/ozone/lib/*.jar")
		for _, j := range jars {
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: ozone <command> [args...]")
		fmt.Fprintln(os.Stderr, "Commands: datanode, om, scm, s3g, recon, scm-start, om-start, idle")
		os.Exit(1)
	}

	command := os.Args[1]
	env := prepareEnv()

	switch command {

	// idle — бесконечное ожидание для утилитарных контейнеров (CLI) в distroless-образах.
	case "idle":
		select {}

	// scm-start — полная последовательность запуска SCM без shell:
	//   1. Только на примордиальном узле (POD_NAME оканчивается на "-0-0"):
	//      если VERSION-файл отсутствует — выполнить ozone scm --init
	//   2. Всегда — ozone scm --bootstrap
	//   3. exec ozone scm
	case "scm-start":
		classpath := buildClasspath()
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
		classpath := buildClasspath()
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
			fmt.Fprintln(os.Stderr, "Available commands: datanode, om, scm, s3g, recon, scm-start, om-start, idle")
			os.Exit(1)
		}
		classpath := buildClasspath()
		execJava(mainClass, classpath, os.Args[2:], env)
	}
}
