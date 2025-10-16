package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type terminalMode int

const (
	TermBottomHidden terminalMode = iota
	TermCompact
	TermExpanded
	TermHiddenTop
)

type model struct {
	width, height int

	leftDir, rightDir       string
	leftItems, rightItems   []string
	activePanel             int
	leftCursor, rightCursor int
	leftScroll, rightScroll int

	terminalMode     terminalMode
	terminalHeight   int
	targetTermHeight int
	termAnimating    bool

	termOutput      []string
	termInput       textinput.Model
	focusOnTerminal bool
	showHiddenLeft  bool
	showHiddenRight bool

	// clipboard for copy/move
	clipboard []string
	operation string // "copy" or "move"

	// rename state
	renaming      bool
	renameInput   textinput.Model
	renameTarget  string
	renamePanel   int
	renameOldPath string

	copying     bool
	copyPercent int
	copyFile    string

	flashMessage string
	flashTimer   time.Time

	// selection maps per panel
	selectedLeft  map[string]bool
	selectedRight map[string]bool
}

type tickMsg time.Time

type copyProgressMsg struct {
	Filename string
	Percent  int
}

type copyDoneMsg struct {
	Filename string
	Success  bool
	Error    error
}

type runCommandMsg struct {
	Command string
	Output  string
	Error   error
}

func initialModel() model {
	ti := textinput.New()
	ti.Placeholder = "$ "
	ti.CharLimit = 256
	ti.Width = 30

	currentDir, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}

	showHiddenLeft := false
	showHiddenRight := false

	leftItems := getDirItems(currentDir, showHiddenLeft)
	rightItems := getDirItems(currentDir, showHiddenRight)

	return model{
		leftDir:          currentDir,
		rightDir:         currentDir,
		leftItems:        leftItems,
		rightItems:       rightItems,
		showHiddenLeft:   showHiddenLeft,
		showHiddenRight:  showHiddenRight,
		terminalMode:     TermCompact,
		terminalHeight:   6,
		targetTermHeight: 6,
		termOutput: []string{
			"Welcome to demo terminal.",
			"Type and press Enter to append lines.",
		},
		termInput:       ti,
		clipboard:       []string{},
		operation:       "",
		renaming:        false,
		renameInput:     textinput.New(),
		selectedLeft:    make(map[string]bool),
		selectedRight:   make(map[string]bool),
		flashMessage:    "",
		flashTimer:      time.Time{},
		copying:         false,
		copyPercent:     0,
		copyFile:        "",
		focusOnTerminal: false,
	}
}

func getDirContents(dir string) ([]string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, file := range files {
		names = append(names, file.Name())
	}
	return names, nil
}

func main() {
	p := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if err := p.Start(); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func animateTerminalCmd() tea.Cmd {
	return tea.Tick(time.Millisecond*15, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	// Если в режиме переименования — обрабатываем только ввод туда
	if m.renaming {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "enter":
				newFilename := m.renameInput.Value()
				newPath := filepath.Join(filepath.Dir(m.renameOldPath), newFilename)

				err := os.Rename(m.renameOldPath, newPath)
				if err != nil {
					m.termOutput = append(m.termOutput, "Error renaming: "+err.Error())
				} else {
					m.termOutput = append(m.termOutput, "Renamed to: "+newFilename)
					if m.renamePanel == 0 {
						m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
						m.leftCursor, m.leftScroll = 0, 0
					} else {
						m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
						m.rightCursor, m.rightScroll = 0, 0
					}
				}
				m.renaming = false
				m.renameInput.SetValue("")
			case "esc":
				m.renaming = false
				m.renameInput.SetValue("")
			default:
				m.renameInput, cmd = m.renameInput.Update(msg)
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		key := msg.String()

		if m.focusOnTerminal {
			// Разрешаем глобальные хоткеи, которые должны работать в любом случае
			switch key {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "ctrl+t", "ctrl+up", "ctrl+down", "alt+up", "alt+down":
				// пусть дальше основной switch их обработает
			case "enter":
				// Обработка команды ENTER при фокусе на терминале:
				input := strings.TrimSpace(m.termInput.Value())
				if input != "" {
					// Сначала — builtin cd
					parts := strings.Fields(input)
					if len(parts) > 0 && parts[0] == "cd" {
						var newPath string
						if len(parts) == 1 || parts[1] == "~" {
							newPath = os.Getenv("HOME")
						} else {
							base := m.leftDir
							if m.activePanel == 1 {
								base = m.rightDir
							}
							newPath = parts[1]
							if !filepath.IsAbs(newPath) {
								newPath = filepath.Join(base, newPath)
							}
						}
						if fi, err := os.Stat(newPath); err == nil && fi.IsDir() {
							if m.activePanel == 0 {
								m.leftDir = newPath
								m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
								m.leftCursor, m.leftScroll = 0, 0
							} else {
								m.rightDir = newPath
								m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
								m.rightCursor, m.rightScroll = 0, 0
							}
							m.termOutput = append(m.termOutput, fmt.Sprintf("$ %s\n--> cd %s", input, newPath))
						} else {
							m.termOutput = append(m.termOutput, fmt.Sprintf("$ %s\ncd: no such directory: %s", input, newPath))
						}
						m.termInput.SetValue("")
						return m, nil
					}

					// Иначе — выполняем разрешённую внешнюю команду асинхронно
					m.termOutput = append(m.termOutput, "$ "+input)
					workingDir := m.leftDir
					if m.activePanel == 1 {
						workingDir = m.rightDir
					}
					cmds = append(cmds, runCommandAsync(input, workingDir))
					m.termInput.SetValue("")
				}
				return m, tea.Batch(cmds...)
			default:
				// всё остальное (включая стрелки) направляем в текстовый инпут
				m.termInput, cmd = m.termInput.Update(msg)
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			}
		}

		switch key {
		case "ctrl+c", "q":
			return m, tea.Quit

		// ПРОБЕЛ — выделение/снятие выделения
		case " ":
			if m.activePanel == 0 {
				if len(m.leftItems) > 0 {
					selected := m.leftItems[m.leftCursor]
					if m.selectedLeft[selected] {
						delete(m.selectedLeft, selected)
					} else {
						m.selectedLeft[selected] = true
					}
				}
			} else {
				if len(m.rightItems) > 0 {
					selected := m.rightItems[m.rightCursor]
					if m.selectedRight[selected] {
						delete(m.selectedRight, selected)
					} else {
						m.selectedRight[selected] = true
					}
				}
			}

		case "p": // Вставить
			if len(m.clipboard) > 0 {
				var destDir string
				if m.activePanel == 0 {
					destDir = m.leftDir
				} else {
					destDir = m.rightDir
				}

				for _, sourceFile := range m.clipboard {
					destPath := filepath.Join(destDir, filepath.Base(sourceFile))

					switch m.operation {
					case "copy":
						ctx := context.Background()
						cmd := copyFileAsync(ctx, sourceFile, destPath)
						cmds = append(cmds, cmd)
						m.termOutput = append(m.termOutput, fmt.Sprintf("Started copying %s → %s", filepath.Base(sourceFile), destDir))
					case "move":
						err := os.Rename(sourceFile, destPath)
						if err != nil {
							m.termOutput = append(m.termOutput, "Error moving: "+err.Error())
						} else {
							m.termOutput = append(m.termOutput, "Moved to: "+destPath)
							// обновляем панели
							m.refreshPanelsAfterChange(filepath.Dir(sourceFile))
							m.refreshPanelsAfterChange(destDir)
						}
					}
				}

				// Обновляем списки
				m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
				m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)

				// Очищаем состояния
				m.clipboard = []string{}
				m.operation = ""
				m.selectedLeft = make(map[string]bool)
				m.selectedRight = make(map[string]bool)

				// Обновляем панели для отображения
				m.refreshPanelsAfterChange(destDir)
			}

		case "c": // Копировать (в буфер)
			m.clipboard = []string{}
			if m.activePanel == 0 {
				if len(m.selectedLeft) > 0 {
					for name := range m.selectedLeft {
						m.clipboard = append(m.clipboard, filepath.Join(m.leftDir, name))
					}
				} else if len(m.leftItems) > 0 {
					selected := m.leftItems[m.leftCursor]
					m.clipboard = append(m.clipboard, filepath.Join(m.leftDir, selected))
				}
			} else {
				if len(m.selectedRight) > 0 {
					for name := range m.selectedRight {
						m.clipboard = append(m.clipboard, filepath.Join(m.rightDir, name))
					}
				} else if len(m.rightItems) > 0 {
					selected := m.rightItems[m.rightCursor]
					m.clipboard = append(m.clipboard, filepath.Join(m.rightDir, selected))
				}
			}
			m.operation = "copy"
			m.termOutput = append(m.termOutput, "Copied to clipboard.")
			// Очистим выделения после копирования в буфер
			m.selectedLeft = make(map[string]bool)
			m.selectedRight = make(map[string]bool)

		case "m": // Переместить (в буфер)
			m.clipboard = []string{}
			if m.activePanel == 0 {
				if len(m.selectedLeft) > 0 {
					for name := range m.selectedLeft {
						m.clipboard = append(m.clipboard, filepath.Join(m.leftDir, name))
					}
				} else if len(m.leftItems) > 0 {
					selected := m.leftItems[m.leftCursor]
					m.clipboard = append(m.clipboard, filepath.Join(m.leftDir, selected))
				}
			} else {
				if len(m.selectedRight) > 0 {
					for name := range m.selectedRight {
						m.clipboard = append(m.clipboard, filepath.Join(m.rightDir, name))
					}
				} else if len(m.rightItems) > 0 {
					selected := m.rightItems[m.rightCursor]
					m.clipboard = append(m.clipboard, filepath.Join(m.rightDir, selected))
				}
			}
			m.operation = "move"
			m.termOutput = append(m.termOutput, "Ready to move.")
			// Очистим выделения
			m.selectedLeft = make(map[string]bool)
			m.selectedRight = make(map[string]bool)

		case "D": // Удалить (поддерживает множественное выделение)
			var targets []string
			if m.activePanel == 0 {
				if len(m.selectedLeft) > 0 {
					for name := range m.selectedLeft {
						targets = append(targets, filepath.Join(m.leftDir, name))
					}
				} else if len(m.leftItems) > 0 {
					targets = append(targets, filepath.Join(m.leftDir, m.leftItems[m.leftCursor]))
				}
			} else {
				if len(m.selectedRight) > 0 {
					for name := range m.selectedRight {
						targets = append(targets, filepath.Join(m.rightDir, name))
					}
				} else if len(m.rightItems) > 0 {
					targets = append(targets, filepath.Join(m.rightDir, m.rightItems[m.rightCursor]))
				}
			}

			if len(targets) == 0 {
				m.termOutput = append(m.termOutput, "Nothing to delete.")
			} else {
				for _, t := range targets {
					err := os.RemoveAll(t)
					if err != nil {
						m.termOutput = append(m.termOutput, "Error deleting "+t+": "+err.Error())
					} else {
						m.termOutput = append(m.termOutput, "Deleted: "+filepath.Base(t))
					}
				}
				// Обновляем панели после удаления
				m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
				m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
				m.leftCursor, m.leftScroll = 0, 0
				m.rightCursor, m.rightScroll = 0, 0
				// Очистим выделения
				m.selectedLeft = make(map[string]bool)
				m.selectedRight = make(map[string]bool)
			}

		case "r": // Переименовать
			// Инициализируем состояние переименования
			m.renaming = true

			// Определяем какая панель активна
			if m.activePanel == 0 {
				selected := m.leftItems[m.leftCursor]
				m.renameOldPath = filepath.Join(m.leftDir, selected)
				m.renamePanel = 0

			} else {
				selected := m.rightItems[m.rightCursor]
				m.renameOldPath = filepath.Join(m.rightDir, selected)
				m.renamePanel = 1
			}

			// Устанавливаем параметры для поля ввода переименования
			m.renameInput = textinput.New()
			m.renameInput.Placeholder = filepath.Base(m.renameOldPath)
			m.renameInput.Focus()
			m.renameInput.CharLimit = 256
			m.renameInput.Width = 30

			cmd = m.renameInput.Focus()
			cmds = append(cmds, cmd)

		case "x":
			m.clipboard = []string{}
			m.operation = ""
			m.termOutput = append(m.termOutput, "Clipboard cleared.")
			// очистить выделения тоже на всякий
			m.selectedLeft = make(map[string]bool)
			m.selectedRight = make(map[string]bool)

		case ".":
			if m.activePanel == 0 {
				m.showHiddenLeft = !m.showHiddenLeft
				m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
			} else {
				m.showHiddenRight = !m.showHiddenRight
				m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
			}

		case "alt+left":
			m.activePanel = 0
		case "alt+right":
			m.activePanel = 1
		case "alt+up", "alt+down":
			m.focusOnTerminal = !m.focusOnTerminal

		case "left":
			if m.activePanel == 0 {
				newPath := filepath.Dir(m.leftDir)
				if newPath != m.leftDir {
					m.leftDir = newPath
					m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
					m.leftCursor, m.leftScroll = 0, 0
				}
			} else {
				newPath := filepath.Dir(m.rightDir)
				if newPath != m.rightDir {
					m.rightDir = newPath
					m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
					m.rightCursor, m.rightScroll = 0, 0
				}
			}

		case "right":
			// Вход в директорию или использование буфера (поддержка множественных элементов)
			if m.activePanel == 0 {
				if len(m.leftItems) == 0 {
					break
				}
				selected := m.leftItems[m.leftCursor]
				newPath := filepath.Join(m.leftDir, selected)
				fileInfo, err := os.Stat(newPath)
				if err == nil && fileInfo.IsDir() {
					m.leftDir = newPath
					m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
					m.leftCursor, m.leftScroll = 0, 0
				} else {
					// Если файл и в буфере есть элементы — вставляем все
					if len(m.clipboard) > 0 {
						destDir := m.leftDir
						for _, sourceFile := range m.clipboard {
							destPath := filepath.Join(destDir, filepath.Base(sourceFile))
							switch m.operation {
							case "copy":
								err := copyFile(sourceFile, destPath)
								if err != nil {
									m.termOutput = append(m.termOutput, "Error copying: "+err.Error())
								} else {
									m.termOutput = append(m.termOutput, "Copied to: "+destPath)
									m.refreshPanelsAfterChange(destDir)
								}
							case "move":
								err := os.Rename(sourceFile, destPath)
								if err != nil {
									m.termOutput = append(m.termOutput, "Error moving: "+err.Error())
								} else {
									m.termOutput = append(m.termOutput, "Moved to: "+destPath)
									m.refreshPanelsAfterChange(filepath.Dir(sourceFile))
									m.refreshPanelsAfterChange(destDir)
								}
							}
						}
						m.clipboard = []string{}
						m.operation = ""
						m.selectedLeft = make(map[string]bool)
						m.selectedRight = make(map[string]bool)
					} else {
						m.termOutput = append(m.termOutput, "Run: "+newPath)
					}
				}
			} else {
				if len(m.rightItems) == 0 {
					break
				}
				selected := m.rightItems[m.rightCursor]
				newPath := filepath.Join(m.rightDir, selected)
				fileInfo, err := os.Stat(newPath)
				if err == nil && fileInfo.IsDir() {
					m.rightDir = newPath
					m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
					m.rightCursor, m.rightScroll = 0, 0
				} else {
					if len(m.clipboard) > 0 {
						destDir := m.rightDir
						for _, sourceFile := range m.clipboard {
							destPath := filepath.Join(destDir, filepath.Base(sourceFile))
							switch m.operation {
							case "copy":
								err := copyFile(sourceFile, destPath)
								if err != nil {
									m.termOutput = append(m.termOutput, "Error copying: "+err.Error())
								} else {
									m.termOutput = append(m.termOutput, "Copied to: "+destPath)
									m.refreshPanelsAfterChange(destDir)
								}
							case "move":
								err := os.Rename(sourceFile, destPath)
								if err != nil {
									m.termOutput = append(m.termOutput, "Error moving: "+err.Error())
								} else {
									m.termOutput = append(m.termOutput, "Moved to: "+destPath)
									m.refreshPanelsAfterChange(filepath.Dir(sourceFile))
									m.refreshPanelsAfterChange(destDir)
								}
							}
						}
						m.clipboard = []string{}
						m.operation = ""
						m.selectedLeft = make(map[string]bool)
						m.selectedRight = make(map[string]bool)
					} else {
						m.termOutput = append(m.termOutput, "Run: "+newPath)
					}
				}
			}

		case "up":
			if m.activePanel == 0 {
				if m.leftCursor > 0 {
					m.leftCursor--
				}
				if m.leftCursor < m.leftScroll {
					m.leftScroll = m.leftCursor
				}
			} else {
				if m.rightCursor > 0 {
					m.rightCursor--
				}
				if m.rightCursor < m.rightScroll {
					m.rightScroll = m.rightCursor
				}
			}

		case "down":
			if m.activePanel == 0 {
				if m.leftCursor < len(m.leftItems)-1 {
					m.leftCursor++
				}
				m.adjustScroll()
			} else {
				if m.rightCursor < len(m.rightItems)-1 {
					m.rightCursor++
				}
				m.adjustScroll()
			}

		case "ctrl+up":
			m.targetTermHeight++
			if m.targetTermHeight > m.height-3 {
				m.targetTermHeight = m.height - 3
			}
			m.termAnimating = true
			cmds = append(cmds, animateTerminalCmd())

		case "ctrl+down":
			if m.targetTermHeight > 0 {
				m.targetTermHeight--
			}
			m.termAnimating = true
			cmds = append(cmds, animateTerminalCmd())

		case "ctrl+t":
			m.terminalMode = (m.terminalMode + 1) % 4
			switch m.terminalMode {
			case TermBottomHidden:
				m.targetTermHeight = 0
			case TermCompact:
				m.targetTermHeight = 6
			case TermExpanded:
				m.targetTermHeight = m.height / 2
			case TermHiddenTop:
				m.targetTermHeight = 1
			}
			m.termAnimating = true
			cmds = append(cmds, animateTerminalCmd())

		case "enter":
			if m.focusOnTerminal {
				// второй обработчик ENTER (в старом месте) — тоже учитываем builtin cd
				input := strings.TrimSpace(m.termInput.Value())
				if input != "" {
					parts := strings.Fields(input)
					if len(parts) > 0 && parts[0] == "cd" {
						var newPath string
						if len(parts) == 1 || parts[1] == "~" {
							newPath = os.Getenv("HOME")
						} else {
							base := m.leftDir
							if m.activePanel == 1 {
								base = m.rightDir
							}
							newPath = parts[1]
							if !filepath.IsAbs(newPath) {
								newPath = filepath.Join(base, newPath)
							}
						}
						if fi, err := os.Stat(newPath); err == nil && fi.IsDir() {
							if m.activePanel == 0 {
								m.leftDir = newPath
								m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
								m.leftCursor, m.leftScroll = 0, 0
							} else {
								m.rightDir = newPath
								m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
								m.rightCursor, m.rightScroll = 0, 0
							}
							m.termOutput = append(m.termOutput, fmt.Sprintf("$ %s\n--> cd %s", input, newPath))
						} else {
							m.termOutput = append(m.termOutput, fmt.Sprintf("$ %s\ncd: no such directory: %s", input, newPath))
						}
						m.termInput.SetValue("")
					} else {
						m.termOutput = append(m.termOutput, "$ "+input)
						workingDir := m.leftDir
						if m.activePanel == 1 {
							workingDir = m.rightDir
						}
						cmds = append(cmds, runCommandAsync(input, workingDir))
						m.termInput.SetValue("")
					}
				}
			}
		default:
			if m.focusOnTerminal {
				m.termInput, cmd = m.termInput.Update(msg)
				cmds = append(cmds, cmd)
				return m, tea.Batch(cmds...)
			}
		}

	case copyProgressMsg:
		m.copying = true
		m.copyFile = msg.Filename
		m.copyPercent = msg.Percent
		if msg.Percent%10 == 0 {
			m.termOutput = append(m.termOutput, fmt.Sprintf("Copying %s... %d%%", msg.Filename, msg.Percent))
		}

	case copyDoneMsg:
		m.copying = false
		if msg.Success {
			m.termOutput = append(m.termOutput, fmt.Sprintf("Copied %s successfully!", msg.Filename))
			destDir := filepath.Dir(msg.Filename)
			m.refreshPanelsAfterChange(destDir)
			m.flashMessage = fmt.Sprintf("Copied: %s", msg.Filename)
			m.flashTimer = time.Now()
		} else {
			m.termOutput = append(m.termOutput, fmt.Sprintf("Failed to copy %s: %v", msg.Filename, msg.Error))
			m.flashMessage = fmt.Sprintf("Error copying %s", msg.Filename)
			m.flashTimer = time.Now()
		}

	case runCommandMsg:
		// Добавляем вывод команды в терминал
		if msg.Error != nil {
			m.termOutput = append(m.termOutput, fmt.Sprintf("Error: %v", msg.Error))
		}
		if msg.Output != "" {
			// Разбиваем вывод на строки и добавляем каждую строку
			lines := strings.Split(strings.TrimRight(msg.Output, "\n"), "\n")
			m.termOutput = append(m.termOutput, lines...)
		}

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.targetTermHeight > m.height {
			m.targetTermHeight = m.height
		}
		if m.terminalMode == TermExpanded {
			m.targetTermHeight = m.height / 2
		}

	case tickMsg:
		if m.termAnimating {
			diff := m.targetTermHeight - m.terminalHeight
			if diff == 0 {
				m.termAnimating = false
			} else {
				step := diff / 4
				if step == 0 {
					if diff > 0 {
						step = 1
					} else {
						step = -1
					}
				}
				m.terminalHeight += step
				cmds = append(cmds, animateTerminalCmd())
			}
		}
	}

	return m, tea.Batch(cmds...)
}

// copyFile копирует файл или директорию (включая вложенные)
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("readdir %s: %w", src, err)
		}
		if len(entries) == 0 {
			return nil
		}
		for _, entry := range entries {
			srcPath := filepath.Join(src, entry.Name())
			dstPath := filepath.Join(dst, entry.Name())
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("mkdir parent %s: %w", dst, err)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer dstFile.Close()

	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(dstFile, srcFile, buf); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}

	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}

	return nil
}

// copyFileAsync выполняет копирование файла в фоне и шлёт сообщения о прогрессе.
func copyFileAsync(ctx context.Context, src, dst string) tea.Cmd {
	return func() tea.Msg {
		srcInfo, err := os.Lstat(src)
		if err != nil {
			return copyDoneMsg{Filename: src, Success: false, Error: err}
		}

		if srcInfo.IsDir() {
			if err := copyFile(src, dst); err != nil {
				return copyDoneMsg{Filename: src, Success: false, Error: err}
			}
			return copyDoneMsg{Filename: src, Success: true}
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return copyDoneMsg{Filename: src, Success: false, Error: err}
		}

		source, err := os.Open(src)
		if err != nil {
			return copyDoneMsg{Filename: src, Success: false, Error: err}
		}
		defer source.Close()

		destination, err := os.Create(dst)
		if err != nil {
			return copyDoneMsg{Filename: src, Success: false, Error: err}
		}
		defer destination.Close()

		if _, err := io.Copy(destination, source); err != nil {
			return copyDoneMsg{Filename: src, Success: false, Error: err}
		}

		if srcInfo.Mode() != 0 {
			_ = os.Chmod(dst, srcInfo.Mode().Perm())
		}

		return copyDoneMsg{Filename: dst, Success: true}
	}
}

// runCommandAsync выполняет команду оболочки в фоне и возвращает результат.
func runCommandAsync(command string, workingDir string) tea.Cmd {
	return func() tea.Msg {
		cmdStr := strings.TrimSpace(command)
		if cmdStr == "" {
			return runCommandMsg{
				Command: command,
				Output:  "",
				Error:   fmt.Errorf("empty command"),
			}
		}

		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			return runCommandMsg{
				Command: command,
				Output:  "",
				Error:   fmt.Errorf("empty command"),
			}
		}

		cmdName := parts[0]
		args := parts[1:]

		// НЕ запускаем shell (sh -c) — запускаем конкретную команду.
		// whitelist — добавляйте сюда только безопасные команды, которые хотите разрешить.
		allowed := map[string]bool{
			"ls":   true,
			"pwd":  true,
			"cat":  true,
			"echo": true,
			"head": true,
			"tail": true,
			"stat": true,
			"date": true,
		}

		// Если команда — cd, сообщаем об этом как ошибке: cd обрабатывается в приложении как builtin.
		if cmdName == "cd" {
			return runCommandMsg{
				Command: command,
				Output:  "",
				Error:   fmt.Errorf("cd is a builtin and handled by the application"),
			}
		}

		if !allowed[cmdName] {
			return runCommandMsg{
				Command: command,
				Output:  fmt.Sprintf("command not allowed: %s", cmdName),
				Error:   nil,
			}
		}

		// Таймаут для команды — чтобы не зависала надолго
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, cmdName, args...)
		cmd.Dir = workingDir

		output, err := cmd.CombinedOutput()
		outStr := string(output)

		// Если был таймаут, заменим ошибку понятной строкой
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("command timed out")
		}

		// Ограничим вывод, чтобы не засорять память/интерфейс
		const maxOut = 20000
		if len(outStr) > maxOut {
			outStr = outStr[:maxOut] + "\n...[output truncated]"
		}

		return runCommandMsg{
			Command: command,
			Output:  outStr,
			Error:   err,
		}
	}
}

func (m *model) refreshPanelsAfterChange(changedDir string) {
	// Обновляем левую панель, если путь совпадает или вложен
	if strings.HasPrefix(changedDir, m.leftDir) || changedDir == m.leftDir {
		m.leftItems = getDirItems(m.leftDir, m.showHiddenLeft)
	}
	// Обновляем правую панель
	if strings.HasPrefix(changedDir, m.rightDir) || changedDir == m.rightDir {
		m.rightItems = getDirItems(m.rightDir, m.showHiddenRight)
	}
}

func (m *model) adjustScroll() {
	panelH := m.height - m.terminalHeight
	if panelH < 3 {
		panelH = 3
	}

	maxVisible := panelH - 8
	if maxVisible < 1 {
		maxVisible = 1
	}

	if m.activePanel == 0 {
		if m.leftCursor >= m.leftScroll+maxVisible {
			m.leftScroll = m.leftCursor - maxVisible + 1
		}
	} else {
		if m.rightCursor >= m.rightScroll+maxVisible {
			m.rightScroll = m.rightCursor - maxVisible + 1
		}
	}
}

func (m model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Loading..."
	}

	if m.renaming {
		return m.renderRenamePopup()
	}

	panelH := m.height - m.terminalHeight
	if panelH < 1 {
		panelH = 1
	}
	panelW := m.width/2 - 2
	if panelW < 10 {
		panelW = m.width - 4
	}

	left := renderPanel(m.leftDir, m.leftItems, m.selectedLeft, m.activePanel == 0 && !m.focusOnTerminal, panelW, panelH, m.leftCursor, m.leftScroll)
	right := renderPanel(m.rightDir, m.rightItems, m.selectedRight, m.activePanel == 1 && !m.focusOnTerminal, panelW, panelH, m.rightCursor, m.rightScroll)

	var b strings.Builder
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, right))
	if m.terminalMode != TermBottomHidden && m.terminalHeight > 0 {
		b.WriteString("\n" + renderTerminal(m, m.terminalHeight, m.width))
	}

	if m.copying {
		progress := fmt.Sprintf("Copying %s: %d%%", m.copyFile, m.copyPercent)
		b.WriteString("\n" + lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render(progress))
	}

	b.WriteString("\n" + lipgloss.NewStyle().Faint(true).Render("Alt+←/→ switch panels • Alt+↑/↓ focus terminal • Ctrl+↑/↓ resize • Ctrl+T toggle terminal • q quit"))
	return b.String()
}

func (m model) renderRenamePopup() string {
	popupStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("171")).
		Padding(1, 2).
		Width(50)

	title := lipgloss.NewStyle().Bold(true).Render("Rename file")
	inputView := m.renameInput.View()

	content := lipgloss.JoinVertical(lipgloss.Left, title, inputView)
	popup := popupStyle.Render(content)

	popupWidth := 50
	popupHeight := 7

	x := (m.width - popupWidth) / 2
	y := (m.height - popupHeight) / 2

	positionStyle := lipgloss.NewStyle().
		MarginLeft(x).
		MarginTop(y)

	return positionStyle.Render(popup)
}

func getDirItems(dir string, showHidden bool) []string {
	files, err := os.ReadDir(dir)
	if err != nil {
		return []string{"Error: " + err.Error()}
	}
	var items []string
	for _, f := range files {
		name := f.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		items = append(items, name)
	}
	return items
}

func renderPanel(dir string, items []string, selected map[string]bool, active bool, w, h int, cursor int, scroll int) string {
	if w < 10 {
		w = 10
	}
	if h < 1 {
		h = 1
	}

	availableHeight := h - 8
	if availableHeight < 1 {
		availableHeight = 1
	}

	if scroll < 0 {
		scroll = 0
	}
	if scroll > len(items) {
		scroll = len(items)
	}

	end := scroll + availableHeight
	if end > len(items) {
		end = len(items)
	}
	visibleItems := items[scroll:end]

	boxStyle := lipgloss.NewStyle().
		Width(w).
		Height(h-6).
		Padding(0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240"))

	if active {
		boxStyle = boxStyle.BorderForeground(lipgloss.Color("171"))
	}

	title := lipgloss.NewStyle().Bold(true).Render(filepath.Base(dir))

	var body strings.Builder
	for i, item := range visibleItems {
		index := scroll + i
		isSelected := selected[item]

		if index == cursor {
			if isSelected {
				body.WriteString(
					lipgloss.NewStyle().
						Foreground(lipgloss.Color("0")).
						Background(lipgloss.Color("213")).
						Bold(true).
						Render("[*] " + item),
				)
			} else {
				body.WriteString(
					lipgloss.NewStyle().
						Foreground(lipgloss.Color("171")).
						Bold(true).
						Render("● " + item),
				)
			}
		} else {
			if isSelected {
				body.WriteString(
					lipgloss.NewStyle().
						Foreground(lipgloss.Color("213")).
						Render("[*] " + item),
				)
			} else {
				body.WriteString("   " + item)
			}
		}
		body.WriteString("\n")
	}

	content := title + "\n" + body.String()
	return boxStyle.Render(content)
}

func renderTerminal(m model, h, w int) string {
	if h < 1 {
		h = 1
	}
	borderColor := lipgloss.Color("240")
	if m.focusOnTerminal {
		borderColor = lipgloss.Color("171")
	}

	box := lipgloss.NewStyle().
		Width(w-2).
		Height(h).
		Padding(0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor)

	maxLines := h - 3
	if maxLines < 0 {
		maxLines = 0
	}

	outLines := m.termOutput
	if len(outLines) > maxLines {
		outLines = outLines[len(outLines)-maxLines:]
	}
	out := strings.Join(outLines, "\n")

	m.termInput.Width = w - 4
	inputView := m.termInput.View()
	if m.focusOnTerminal {
		inputView = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212")).Render(inputView)
	}

	return box.Render(out + "\n" + inputView)
}
