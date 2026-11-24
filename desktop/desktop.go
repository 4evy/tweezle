// Package desktop rewrites freedesktop desktop entry files
package desktop

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/buildkite/shellwords"
	"gopkg.in/ini.v1"
)

const (
	sectionDesktop    = "Desktop Entry"
	keyExec           = "Exec"
	keyIcon           = "Icon"
	keyStartupNotify  = "StartupNotify"
	keyStartupWMClass = "StartupWMClass"
	keyNoDisplay      = "NoDisplay"
	execReserved      = " \t\n\"'\\><~|&;$*?#()`"
)

// Exec quoting is nested inside desktop-entry value escaping, so backslashes
// pass through both parsers; percent signs escape field codes
var execEscapes = strings.NewReplacer(
	"\\", `\\\\`,
	"\"", `\\"`,
	"`", "\\\\`",
	"$", `\\$`,
	"%", "%%",
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

var desktopValueUnescape = strings.NewReplacer(`\\`, `\`, `\s`, " ", `\t`, "\t", `\r`, "\r", `\n`, "\n")
var desktopExecUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "\\`", "`", `\$`, "$")

// execWord finds one token without reserializing any following field codes or
// whitespace. Flatpak's GLib-generated prefix also uses single quotes
func execWord(text string, flatpak bool) (word, rest string, err error) {
	text = strings.TrimLeft(text, " \t")
	var quote byte
	var escaped bool
	for index := 0; index < len(text); {
		c, width := text[index], 1
		if c == '\\' && index+1 < len(text) && text[index+1] == '\\' {
			// Desktop-entry value decoding precedes Exec quoting
			width = 2
		}
		switch {
		case escaped:
			escaped = false
		case c == '\\' && quote != '\'' && (flatpak || width == 2):
			escaped = true
		case quote != 0:
			if quote == c {
				quote = 0
			}
		case c == '"' || (flatpak && c == '\''):
			quote = c
		case c == ' ' || c == '\t':
			return text[:index], text[index:], nil
		}
		index += width
	}
	if quote != 0 {
		return "", "", fmt.Errorf("unterminated quote in Exec value %q", text)
	}
	return text, "", nil
}

// Rewrite replaces matching executables and applies browser desktop metadata.
func Rewrite(text, executable, sourceExec, startupWMClass string) (string, error) {
	return RewriteWithIcon(text, executable, sourceExec, startupWMClass, "")
}

// RewriteWithIcon replaces matching executables and applies browser desktop
// metadata, including the installed icon theme name when one is provided.
func RewriteWithIcon(
	text,
	executable,
	sourceExec,
	startupWMClass,
	iconName string,
) (string, error) {
	config, err := load(text)
	if err != nil {
		return "", fmt.Errorf("parse desktop entry: %w", err)
	}
	for _, section := range config.Sections() {
		key, err := section.GetKey(keyExec)
		if err != nil {
			continue
		}
		token, rest, err := execWord(key.String(), false)
		if err != nil {
			return "", fmt.Errorf("desktop entry %s: %w", section.Name(), err)
		}
		command := desktopValueUnescape.Replace(token)
		if inner, ok := strings.CutPrefix(command, `"`); ok && strings.HasSuffix(inner, `"`) {
			command = desktopExecUnescape.Replace(strings.TrimSuffix(inner, `"`))
		}
		if strings.ReplaceAll(command, "%%", "%") == sourceExec {
			replacement, err := EscapeExecutable(executable)
			if err != nil {
				return "", err
			}
			key.SetValue(replacement + rest)
		}
	}
	desktop := config.Section(sectionDesktop)
	desktop.Key(keyStartupNotify).SetValue(strconv.FormatBool(false))
	if iconName != "" {
		desktop.Key(keyIcon).SetValue(iconName)
	}
	if startupWMClass != "" {
		desktop.Key(keyStartupWMClass).SetValue(startupWMClass)
	}
	output, err := render(config)
	if err != nil {
		return "", fmt.Errorf("render desktop entry: %w", err)
	}
	return output, nil
}

// Alias marks a desktop entry as hidden from application menus.
func Alias(text string) (string, error) {
	config, err := load(text)
	if err != nil {
		return "", fmt.Errorf("parse desktop entry alias: %w", err)
	}
	config.Section(sectionDesktop).Key(keyNoDisplay).SetValue(strconv.FormatBool(true))
	output, err := render(config)
	if err != nil {
		return "", fmt.Errorf("render desktop entry alias: %w", err)
	}
	return output, nil
}

// RewriteFlatpak replaces the exported flatpak run prefix, retaining browser
// arguments and file-forwarding markers consumed by the new Flatpak launcher
func RewriteFlatpak(text, executable, appID, defaultCommand string) (string, error) {
	config, err := load(text)
	if err != nil {
		return "", err
	}
	replacement, err := EscapeExecutable(executable)
	if err != nil {
		return "", err
	}
	for _, section := range config.Sections() {
		key, err := section.GetKey(keyExec)
		if err != nil {
			continue
		}
		arguments := key.String()
		index := 0
		found := false
		for strings.TrimSpace(arguments) != "" {
			token, rest, err := execWord(arguments, true)
			if err != nil {
				return "", fmt.Errorf("desktop entry %s: %w", section.Name(), err)
			}
			words, err := shellwords.SplitPosix(desktopValueUnescape.Replace(token))
			if err != nil || len(words) != 1 {
				return "", fmt.Errorf("desktop entry %s has an unsupported Flatpak command", section.Name())
			}
			word := words[0]
			arguments = rest
			switch index {
			case 0:
				if filepath.Base(word) != "flatpak" {
					return "", fmt.Errorf("desktop entry %s does not launch Flatpak", section.Name())
				}
			case 1:
				if word != "run" {
					return "", fmt.Errorf("desktop entry %s does not use flatpak run", section.Name())
				}
			default:
				if word == appID {
					found = true
				} else if command, ok := strings.CutPrefix(word, "--command="); ok {
					if command == "" || command != defaultCommand {
						return "", fmt.Errorf("desktop entry %s uses a nondefault Flatpak command %q", section.Name(), command)
					}
				} else if !strings.HasPrefix(word, "--") || word == "--command" {
					return "", fmt.Errorf("desktop entry %s has an unsupported Flatpak argument %q", section.Name(), word)
				}
			}
			if found {
				break
			}
			index++
		}
		if !found {
			return "", fmt.Errorf("desktop entry %s does not launch Flatpak %s", section.Name(), appID)
		}
		key.SetValue(replacement + arguments)
	}
	entry := config.Section(sectionDesktop)
	entry.DeleteKey("TryExec")
	entry.Key("DBusActivatable").SetValue("false")
	return render(config)
}

func load(text string) (*ini.File, error) {
	return ini.LoadSources(ini.LoadOptions{
		Insensitive:         false,
		InsensitiveSections: false,
		InsensitiveKeys:     false,
		IgnoreInlineComment: true,
	}, []byte(text))
}

func render(config *ini.File) (string, error) {
	var output strings.Builder
	if _, err := config.WriteTo(&output); err != nil {
		return "", err
	}
	return output.String(), nil
}

// EscapeExecutable quotes a desktop-entry Exec executable when required.
func EscapeExecutable(executable string) (string, error) {
	if strings.ContainsRune(executable, '=') {
		return "", fmt.Errorf("desktop entry executable contains '=': %s", executable)
	}
	if !strings.ContainsAny(executable, execReserved+"%") {
		return executable, nil
	}
	return `"` + execEscapes.Replace(executable) + `"`, nil
}
