package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Note struct {
	Title   string
	Content string
	Section string
	Path    string // относительный путь без расширения (раздел/название)
}

type Section struct {
	Name  string
	Notes []Note
}

type PageData struct {
	Title    string
	Sections []Section
	Query    string
	Flash    string
}

type SectionEditData struct {
	Title       string
	Section     string
	Notes       []Note
	AllSections map[string][]Note
	Flash       string
}

var (
	notesCache map[string][]Note
	cacheMutex sync.RWMutex
)

const orderFile = "sections_order.json"

// openBrowser открывает URL в браузере по умолчанию
func openBrowser(url string) error {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("cmd", "/c", "start", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("неподдерживаемая операционная система")
	}
	return err
}

// waitForServer ожидает, пока сервер станет доступен
func waitForServer(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		client := http.Client{
			Timeout: 2 * time.Second,
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("сервер не ответил в течение %v", timeout)
}

// loadSectionsOrder загружает сохранённый порядок разделов
func loadSectionsOrder() []string {
	data, err := os.ReadFile(orderFile)
	if err != nil {
		return []string{}
	}
	var order []string
	if err := json.Unmarshal(data, &order); err != nil {
		log.Printf("Ошибка разбора порядка разделов: %v", err)
		return []string{}
	}
	return order
}

// saveSectionsOrder сохраняет порядок разделов
func saveSectionsOrder(order []string) error {
	data, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return os.WriteFile(orderFile, data, 0644)
}

// updateSectionsOrder обновляет порядок после переименования/удаления
func updateSectionsOrder(oldName, newName string) {
	order := loadSectionsOrder()
	newOrder := make([]string, 0, len(order))
	for _, name := range order {
		if name == oldName {
			if newName != "" {
				newOrder = append(newOrder, newName)
			}
		} else {
			newOrder = append(newOrder, name)
		}
	}
	if newName != "" && oldName == "" {
		found := false
		for _, n := range newOrder {
			if n == newName {
				found = true
				break
			}
		}
		if !found {
			newOrder = append(newOrder, newName)
		}
	}
	saveSectionsOrder(newOrder)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.status = code
	lrw.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next(lw, r)
		log.Printf("[%s] %s %s - %d (%v)", r.Method, r.URL.Path, r.RemoteAddr, lw.status, time.Since(start))
	}
}

func main() {
	os.MkdirAll("notes", 0755)
	os.MkdirAll("templates", 0755)
	os.MkdirAll("static", 0755)

	loadNotesCached()

	http.HandleFunc("/", loggingMiddleware(indexHandler))
	http.HandleFunc("/view/", loggingMiddleware(viewHandler))
	http.HandleFunc("/search", loggingMiddleware(searchHandler))
	http.HandleFunc("/create", loggingMiddleware(createHandler))
	http.HandleFunc("/save", loggingMiddleware(saveHandler))
	http.HandleFunc("/edit/", loggingMiddleware(editHandler))
	http.HandleFunc("/update", loggingMiddleware(updateHandler))
	http.HandleFunc("/delete/", loggingMiddleware(deleteHandler))
	http.HandleFunc("/delete-section/", loggingMiddleware(deleteSectionHandler))
	http.HandleFunc("/edit-section/", loggingMiddleware(editSectionHandler))
	http.HandleFunc("/rename-section", loggingMiddleware(renameSectionHandler))
	http.HandleFunc("/move-note", loggingMiddleware(moveNoteHandler))
	http.HandleFunc("/reorder-sections", loggingMiddleware(reorderSectionsHandler))

	http.HandleFunc("/import-export", loggingMiddleware(importExportHandler))
	http.HandleFunc("/api/categories", loggingMiddleware(categoriesHandler))
	http.HandleFunc("/export/txt", loggingMiddleware(exportTxtHandler))
	http.HandleFunc("/export/doc", loggingMiddleware(exportDocHandler))
	http.HandleFunc("/import", loggingMiddleware(importHandler))

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	serverURL := "http://localhost:8080"
	log.Printf("Сервер запускается на %s", serverURL)

	// Запускаем сервер в отдельной горутине
	server := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	// Запускаем сервер
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Ошибка запуска сервера: %v", err)
		}
	}()

	// Ждем, пока сервер станет доступен
	log.Println("Ожидание запуска сервера...")
	if err := waitForServer(serverURL, 5*time.Second); err != nil {
		log.Printf("Предупреждение: %v", err)
	} else {
		log.Println("Сервер успешно запущен")
	}

	// Открываем браузер
	log.Printf("Открытие браузера: %s", serverURL)
	if err := openBrowser(serverURL); err != nil {
		log.Printf("Не удалось открыть браузер автоматически: %v", err)
		log.Printf("Пожалуйста, откройте вручную: %s", serverURL)
	} else {
		log.Println("Браузер открыт автоматически")
	}

	// Ожидаем завершения (блокируем главную горутину)
	select {}
}

// reorderSectionsHandler сохраняет новый порядок разделов
func reorderSectionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	var order []string
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := saveSectionsOrder(order); err != nil {
		log.Printf("Ошибка сохранения порядка разделов: %v", err)
		http.Error(w, "Ошибка сохранения порядка", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func loadNotes() map[string][]Note {
	notes := make(map[string][]Note)
	err := filepath.Walk("notes", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == "notes" || info.IsDir() || filepath.Ext(path) != ".txt" {
			return nil
		}
		relPath, err := filepath.Rel("notes", path)
		if err != nil {
			return err
		}
		section := filepath.Dir(relPath)
		if section == "." {
			section = "Общие"
		}
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Ошибка чтения файла %s: %v", path, err)
			return nil
		}
		title := strings.TrimSuffix(filepath.Base(path), ".txt")
		note := Note{
			Title:   title,
			Content: string(content),
			Section: section,
			Path:    strings.TrimSuffix(relPath, ".txt"),
		}
		notes[section] = append(notes[section], note)
		return nil
	})
	if err != nil {
		log.Printf("Ошибка загрузки заметок: %v", err)
	}
	log.Printf("loadNotes: найдено %d разделов", len(notes))
	for section, list := range notes {
		log.Printf("  раздел '%s' содержит %d заметок", section, len(list))
	}
	return notes
}

func loadNotesCached() map[string][]Note {
	cacheMutex.RLock()
	if notesCache != nil {
		defer cacheMutex.RUnlock()
		return notesCache
	}
	cacheMutex.RUnlock()
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	if notesCache == nil {
		notesCache = loadNotes()
	}
	return notesCache
}

func updateNotesCache() {
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	notesCache = loadNotes()
}

func setFlash(w http.ResponseWriter, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    url.QueryEscape(message),
		Path:     "/",
		MaxAge:   1,
		HttpOnly: true,
	})
}

func getFlash(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie("flash")
	if err != nil {
		return ""
	}
	message, _ := url.QueryUnescape(cookie.Value)
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	return message
}

func isValidFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	forbiddenChars := `<>:"/\|?*`
	return !strings.ContainsAny(name, forbiddenChars)
}

// mapToSections преобразует map в упорядоченный срез Section
func mapToSections(notesMap map[string][]Note) []Section {
	order := loadSectionsOrder()
	sections := make([]Section, 0, len(order)+len(notesMap))
	// Добавляем разделы по порядку
	for _, name := range order {
		if notes, ok := notesMap[name]; ok {
			sections = append(sections, Section{Name: name, Notes: notes})
			delete(notesMap, name)
		}
	}
	// Добавляем оставшиеся разделы
	for name, notes := range notesMap {
		sections = append(sections, Section{Name: name, Notes: notes})
	}
	return sections
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	notesMap := loadNotesCached()
	sections := mapToSections(notesMap)
	data := PageData{
		Title:    "NoteZone",
		Sections: sections,
		Flash:    getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендеринга index.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func viewHandler(w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimPrefix(r.URL.Path, "/view/")
	if rawPath == "" {
		http.NotFound(w, r)
		return
	}
	decodedPath, err := url.QueryUnescape(rawPath)
	if err != nil {
		decodedPath = rawPath
	}
	parts := strings.SplitN(decodedPath, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "Некорректный формат пути. Ожидается: /view/раздел/название", http.StatusBadRequest)
		return
	}
	section := parts[0]
	noteTitle := parts[1]

	fullPath := filepath.Join("notes", section, noteTitle+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}

	data := struct {
		Title   string
		Section string
		Content string
		Path    string
	}{
		Title:   noteTitle,
		Section: section,
		Content: string(content),
		Path:    decodedPath,
	}
	tmpl := template.Must(template.ParseFiles("templates/view.html"))
	tmpl.Execute(w, data)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notePath := strings.TrimPrefix(r.URL.Path, "/delete/")
	if notePath == "" {
		http.NotFound(w, r)
		return
	}
	decodedPath, err := url.QueryUnescape(notePath)
	if err != nil {
		decodedPath = notePath
	}
	fullPath := filepath.Join("notes", decodedPath+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	if err := os.Remove(fullPath); err != nil {
		http.Error(w, "Ошибка удаления файла", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	setFlash(w, "Заметка успешно удалена")
	w.WriteHeader(http.StatusOK)
}

func createHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notes := loadNotesCached()
	sections := make([]string, 0, len(notes))
	for section := range notes {
		sections = append(sections, section)
	}
	funcMap := template.FuncMap{
		"js": func(s interface{}) string {
			return template.JSEscapeString(fmt.Sprintf("%v", s))
		},
	}
	data := map[string]interface{}{
		"Title":    "Создать новую заметку",
		"Sections": sections,
		"Flash":    getFlash(w, r),
	}
	tmpl := template.Must(template.New("create.html").Funcs(funcMap).ParseFiles("templates/create.html"))
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендеринга create.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	section := r.FormValue("section")
	newSection := r.FormValue("new_section")
	title := r.FormValue("title")
	content := r.FormValue("content")

	finalSection := section
	if section == "" && newSection != "" {
		finalSection = newSection
	}
	if finalSection == "" || title == "" || content == "" {
		http.Error(w, "Все поля обязательны для заполнения", http.StatusBadRequest)
		return
	}
	finalSection = strings.Trim(finalSection, "/\\")
	if finalSection == "." {
		finalSection = "Общие"
	}
	if !isValidFilename(title) || !isValidFilename(finalSection) {
		http.Error(w, "Недопустимые символы в названии", http.StatusBadRequest)
		return
	}
	sectionPath := filepath.Join("notes", finalSection)
	if err := os.MkdirAll(sectionPath, 0755); err != nil {
		http.Error(w, "Ошибка создания раздела: "+err.Error(), http.StatusInternalServerError)
		return
	}
	filePath := filepath.Join(sectionPath, title+".txt")
	if _, err := os.Stat(filePath); err == nil {
		http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		http.Error(w, "Ошибка сохранения файла: "+err.Error(), http.StatusInternalServerError)
		return
	}
	updateNotesCache()

	// Добавляем новый раздел в порядок, если его там нет
	order := loadSectionsOrder()
	found := false
	for _, s := range order {
		if s == finalSection {
			found = true
			break
		}
	}
	if !found {
		order = append(order, finalSection)
		saveSectionsOrder(order)
	}

	setFlash(w, "Заметка успешно создана")
	redirectURL := "/view/" + url.QueryEscape(finalSection) + "/" + url.QueryEscape(title)
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func editHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	notePath := strings.TrimPrefix(r.URL.Path, "/edit/")
	if notePath == "" {
		http.NotFound(w, r)
		return
	}
	decodedPath, err := url.QueryUnescape(notePath)
	if err != nil {
		decodedPath = notePath
	}
	parts := strings.SplitN(decodedPath, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "Некорректный формат пути. Ожидается: /edit/раздел/название", http.StatusBadRequest)
		return
	}
	section := parts[0]
	title := parts[1]

	fullPath := filepath.Join("notes", section, title+".txt")
	if !strings.HasPrefix(filepath.Clean(fullPath), filepath.Clean("notes")+string(os.PathSeparator)) {
		http.Error(w, "Недопустимый путь", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}
	notes := loadNotesCached()
	sections := make([]string, 0, len(notes))
	for s := range notes {
		sections = append(sections, s)
	}
	funcMap := template.FuncMap{
		"contains": func(slice []string, item string) bool {
			for _, s := range slice {
				if s == item {
					return true
				}
			}
			return false
		},
		"js": func(s interface{}) string {
			return template.JSEscapeString(fmt.Sprintf("%v", s))
		},
	}
	data := map[string]interface{}{
		"Title":     "Редактировать заметку",
		"Sections":  sections,
		"Section":   section,
		"NoteTitle": title,
		"Content":   string(content),
		"NotePath":  decodedPath,
		"Flash":     getFlash(w, r),
	}
	tmpl := template.Must(template.New("edit.html").Funcs(funcMap).ParseFiles("templates/edit.html"))
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендеринга edit.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	oldPath := r.FormValue("old_path")
	section := r.FormValue("section")
	newSection := r.FormValue("new_section")
	title := r.FormValue("title")
	content := r.FormValue("content")

	finalSection := strings.TrimSpace(section)
	if finalSection == "" {
		finalSection = strings.TrimSpace(newSection)
	}
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if finalSection == "" || title == "" || content == "" {
		http.Error(w, "Все поля обязательны для заполнения", http.StatusBadRequest)
		return
	}
	finalSection = strings.Trim(finalSection, "/\\")
	if finalSection == "." {
		finalSection = "Общие"
	}
	if !isValidFilename(title) || !isValidFilename(finalSection) {
		http.Error(w, "Недопустимые символы в названии", http.StatusBadRequest)
		return
	}
	sectionPath := filepath.Join("notes", finalSection)
	if err := os.MkdirAll(sectionPath, 0755); err != nil {
		http.Error(w, "Ошибка создания раздела", http.StatusInternalServerError)
		return
	}
	newFullPath := filepath.Join(sectionPath, title+".txt")
	oldFullPath := filepath.Join("notes", oldPath+".txt")
	if oldFullPath != newFullPath {
		if _, err := os.Stat(newFullPath); err == nil {
			http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
			return
		}
		if err := os.Remove(oldFullPath); err != nil && !os.IsNotExist(err) {
			http.Error(w, "Ошибка при удалении старого файла", http.StatusInternalServerError)
			return
		}
	}
	if err := os.WriteFile(newFullPath, []byte(content), 0644); err != nil {
		http.Error(w, "Ошибка сохранения файла", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Заметка успешно обновлена"))
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	notesMap := loadNotesCached()
	resultsMap := make(map[string][]Note)
	for section, sectionNotes := range notesMap {
		for _, note := range sectionNotes {
			if strings.Contains(strings.ToLower(note.Title+note.Content), strings.ToLower(query)) {
				resultsMap[section] = append(resultsMap[section], note)
			}
		}
	}
	sections := mapToSections(resultsMap)
	data := PageData{
		Title:    "Результаты поиска: " + query,
		Sections: sections,
		Query:    query,
		Flash:    getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/index.html"))
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендеринга search: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func deleteSectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	sectionName := strings.TrimPrefix(r.URL.Path, "/delete-section/")
	if sectionName == "" {
		http.Error(w, "Имя раздела обязательно", http.StatusBadRequest)
		return
	}
	var err error
	sectionName, err = url.QueryUnescape(sectionName)
	if err != nil {
		http.Error(w, "Некорректное имя раздела", http.StatusBadRequest)
		return
	}
	if sectionName == "" || sectionName == "." || sectionName == ".." || strings.Contains(sectionName, "..") {
		http.Error(w, "Недопустимое имя раздела", http.StatusBadRequest)
		return
	}
	if sectionName == "Общие" {
		http.Error(w, "Нельзя удалить раздел 'Общие'", http.StatusForbidden)
		return
	}
	sectionPath := filepath.Join("notes", sectionName)
	if _, err := os.Stat(sectionPath); os.IsNotExist(err) {
		http.Error(w, "Раздел не найден", http.StatusNotFound)
		return
	}
	if err := os.RemoveAll(sectionPath); err != nil {
		http.Error(w, "Ошибка удаления раздела", http.StatusInternalServerError)
		return
	}
	// Удаляем из порядка
	order := loadSectionsOrder()
	newOrder := make([]string, 0, len(order))
	for _, name := range order {
		if name != sectionName {
			newOrder = append(newOrder, name)
		}
	}
	saveSectionsOrder(newOrder)
	updateNotesCache()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Раздел успешно удален"))
}

func editSectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	sectionName := strings.TrimPrefix(r.URL.Path, "/edit-section/")
	if sectionName == "" {
		http.NotFound(w, r)
		return
	}
	var err error
	sectionName, err = url.QueryUnescape(sectionName)
	if err != nil {
		http.Error(w, "Некорректное имя раздела", http.StatusBadRequest)
		return
	}
	notes := loadNotesCached()
	if _, exists := notes[sectionName]; !exists && sectionName != "Общие" {
		http.NotFound(w, r)
		return
	}
	data := SectionEditData{
		Title:       "Редактирование раздела: " + sectionName,
		Section:     sectionName,
		Notes:       notes[sectionName],
		AllSections: notes,
		Flash:       getFlash(w, r),
	}
	tmpl := template.Must(template.ParseFiles("templates/edit-section.html"))
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Ошибка рендеринга edit-section.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func renameSectionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	oldSection := r.FormValue("old_section")
	newSection := r.FormValue("new_section")
	if oldSection == "" || newSection == "" {
		http.Error(w, "Имена разделов не могут быть пустыми", http.StatusBadRequest)
		return
	}
	if oldSection == "Общие" {
		http.Error(w, "Нельзя переименовать раздел 'Общие'", http.StatusForbidden)
		return
	}
	if !isValidFilename(newSection) {
		http.Error(w, "Недопустимые символы в названии раздела", http.StatusBadRequest)
		return
	}
	oldPath := filepath.Join("notes", oldSection)
	newPath := filepath.Join("notes", newSection)
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		http.Error(w, "Раздел не найден", http.StatusNotFound)
		return
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		http.Error(w, "Раздел с таким именем уже существует", http.StatusBadRequest)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		http.Error(w, "Ошибка переименования раздела", http.StatusInternalServerError)
		return
	}
	updateSectionsOrder(oldSection, newSection)
	updateNotesCache()
	setFlash(w, "Раздел успешно переименован")
	http.Redirect(w, r, "/edit-section/"+url.QueryEscape(newSection), http.StatusSeeOther)
}

func moveNoteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Ошибка обработки формы", http.StatusBadRequest)
		return
	}
	notePath := r.FormValue("note_path")
	newSection := strings.TrimSpace(r.FormValue("new_section"))
	if notePath == "" || newSection == "" {
		http.Error(w, "Не указаны путь заметки или новый раздел", http.StatusBadRequest)
		return
	}
	decodedNotePath, err := url.QueryUnescape(notePath)
	if err != nil {
		decodedNotePath = notePath
	}
	oldFullPath := filepath.Join("notes", decodedNotePath+".txt")
	newSectionPath := filepath.Join("notes", newSection)
	newFullPath := filepath.Join(newSectionPath, filepath.Base(decodedNotePath)+".txt")
	if _, err := os.Stat(oldFullPath); os.IsNotExist(err) {
		http.Error(w, "Заметка не найдена", http.StatusNotFound)
		return
	}
	if err := os.MkdirAll(newSectionPath, 0755); err != nil {
		http.Error(w, "Ошибка создания раздела", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		http.Error(w, "Ошибка перемещения заметки", http.StatusInternalServerError)
		return
	}
	updateNotesCache()
	setFlash(w, "Заметка успешно перемещена")
	currentSection := filepath.Dir(decodedNotePath)
	if currentSection == "." {
		currentSection = "Общие"
	}
	http.Redirect(w, r, "/edit-section/"+url.QueryEscape(currentSection), http.StatusSeeOther)
}

func importExportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	tmpl := template.Must(template.ParseFiles("templates/import-export.html"))
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("Ошибка рендеринга import-export.html: %v", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
	}
}

func categoriesHandler(w http.ResponseWriter, r *http.Request) {
	notes := loadNotesCached()
	categories := make([]string, 0, len(notes))
	for cat := range notes {
		categories = append(categories, cat)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(categories)
}

func exportTxtHandler(w http.ResponseWriter, r *http.Request) {
	notes := loadNotesCached()
	var buffer bytes.Buffer
	for section, notesList := range notes {
		buffer.WriteString(fmt.Sprintf("=== %s ===\n\n", section))
		for _, note := range notesList {
			buffer.WriteString(fmt.Sprintf("Название: %s\n", note.Title))
			buffer.WriteString(fmt.Sprintf("Содержимое:\n%s\n", note.Content))
			buffer.WriteString("\n---\n\n")
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=notes.txt")
	w.Write(buffer.Bytes())
}

func exportDocHandler(w http.ResponseWriter, r *http.Request) {
	notes := loadNotesCached()
	var rtfBuffer bytes.Buffer
	rtfBuffer.WriteString("{\\rtf1\\ansi\\ansicpg1252\\uc1\\deff0 {\\fonttbl {\\f0 Times New Roman;}}\\f0\\fs24\n")
	rtfEncode := func(s string) string {
		var out strings.Builder
		for _, r := range s {
			if r < 0x80 && r != '\\' && r != '{' && r != '}' {
				out.WriteRune(r)
			} else {
				if r == '\\' || r == '{' || r == '}' {
					out.WriteByte('\\')
					out.WriteRune(r)
				} else {
					out.WriteString(fmt.Sprintf("\\u%d?", r))
				}
			}
		}
		return out.String()
	}
	for section, notesList := range notes {
		sectionTitle := rtfEncode(fmt.Sprintf("=== %s ===", section))
		rtfBuffer.WriteString(fmt.Sprintf("\\b %s \\b0\\par\\par\n", sectionTitle))
		for _, note := range notesList {
			title := rtfEncode(fmt.Sprintf("Название: %s", note.Title))
			rtfBuffer.WriteString(fmt.Sprintf("\\b %s \\b0\\par\n", title))
			content := rtfEncode(note.Content)
			content = strings.ReplaceAll(content, "\r\n", "\n")
			content = strings.ReplaceAll(content, "\n", "\\par\n")
			rtfBuffer.WriteString(content)
			rtfBuffer.WriteString("\\par\\par\n")
			rtfBuffer.WriteString("---\\par\\par\n")
		}
	}
	rtfBuffer.WriteString("}")
	w.Header().Set("Content-Type", "application/msword")
	w.Header().Set("Content-Disposition", "attachment; filename=notes.doc")
	w.Write(rtfBuffer.Bytes())
}

func importHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Ошибка обработки файла", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Файл не передан", http.StatusBadRequest)
		return
	}
	defer file.Close()
	category := strings.TrimSpace(r.FormValue("category"))
	title := strings.TrimSpace(r.FormValue("title"))
	if category == "" || title == "" {
		http.Error(w, "Категория и название обязательны", http.StatusBadRequest)
		return
	}
	if !isValidFilename(category) || !isValidFilename(title) {
		http.Error(w, "Недопустимые символы в названии категории или заметки", http.StatusBadRequest)
		return
	}
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Ошибка чтения файла", http.StatusInternalServerError)
		return
	}
	content := string(fileBytes)
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".txt" && ext != ".doc" {
		http.Error(w, "Поддерживаются только файлы .txt и .doc", http.StatusBadRequest)
		return
	}
	category = strings.Trim(category, "/\\")
	if category == "." {
		category = "Общие"
	}
	categoryPath := filepath.Join("notes", category)
	if err := os.MkdirAll(categoryPath, 0755); err != nil {
		http.Error(w, "Ошибка создания категории", http.StatusInternalServerError)
		return
	}
	fullPath := filepath.Join(categoryPath, title+".txt")
	if _, err := os.Stat(fullPath); err == nil {
		http.Error(w, "Заметка с таким названием уже существует", http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		http.Error(w, "Ошибка сохранения заметки", http.StatusInternalServerError)
		return
	}
	// Добавляем новый раздел в порядок
	order := loadSectionsOrder()
	found := false
	for _, s := range order {
		if s == category {
			found = true
			break
		}
	}
	if !found {
		order = append(order, category)
		saveSectionsOrder(order)
	}
	updateNotesCache()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Заметка успешно импортирована"})
}
