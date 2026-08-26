package importer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/antifreezzz/llmcontrol/internal/db"
)

type ParsedModel struct {
	ModelID        string
	ModelName      string
	EngineBinary   string
	EngineID       string
	ModelPath      string
	MMProjPath     string
	MTPPath        string
	DefaultPort    int
	DefaultProfile string
	Profiles       map[string]ParsedProfile
}

type ParsedProfile struct {
	Name        string
	Description string
	CtxSize     int
	Parallel    int
	KVType      string
	FlashAttn   string
	UseMTP      bool
	UseVision   bool
	EnableUI    bool
	Tools       string
	ExtraArgs   string
}

type ImportResult struct {
	TotalScripts   int
	ImportedModels int
	Errors         []string
}

var (
	reVarAssign    = regexp.MustCompile(`^([A-Za-z0-9_]+)=(.*)$`)
	reProfileDecl  = regexp.MustCompile(`^PROFILES\[([A-Za-z0-9_-]+)\]=(.*)$`)
	reDescrDecl    = regexp.MustCompile(`^PROFILE_DESCR\[([A-Za-z0-9_-]+)\]=(.*)$`)
)

func cleanQuotes(val string) string {
	val = strings.TrimSpace(val)
	if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
		(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
		val = val[1 : len(val)-1]
	}
	return strings.TrimSpace(val)
}

var reVarRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}|\$([A-Za-z0-9_]+)`)

func expandVars(val string, vars map[string]string) string {
	val = cleanQuotes(val)
	return reVarRef.ReplaceAllStringFunc(val, func(match string) string {
		varName := strings.TrimPrefix(match, "$")
		varName = strings.TrimPrefix(varName, "{")
		varName = strings.TrimSuffix(varName, "}")
		if replacement, ok := vars[varName]; ok {
			return replacement
		}
		return match
	})
}

func parseVarAssignments(line string, vars map[string]string) {
	var tokens []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteByte(ch)
		} else if ch == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteByte(ch)
		} else if (ch == ';' || ch == '\n') && !inSingle && !inDouble {
			tokens = append(tokens, current.String())
			current.Reset()
		} else {
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if match := reVarAssign.FindStringSubmatch(token); len(match) == 3 {
			k := match[1]
			v := expandVars(match[2], vars)
			vars[k] = v
		}
	}
}

func ParseScriptContent(filename, content string) (*ParsedModel, error) {
	modelID := strings.TrimSuffix(filename, "-start")
	modelID = strings.TrimSuffix(modelID, ".sh")

	vars := make(map[string]string)
	profileRaw := make(map[string]string)
	profileDescr := make(map[string]string)

	scanner := bufio.NewScanner(strings.NewReader(content))
	var commentHeader []string
	isFirstComments := true

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			if isFirstComments && !strings.HasPrefix(line, "#!") {
				trimmed := strings.TrimSpace(strings.TrimPrefix(line, "#"))
				if trimmed != "" {
					commentHeader = append(commentHeader, trimmed)
				}
			}
			continue
		}
		isFirstComments = false

		// PROFILES[...]
		if match := reProfileDecl.FindStringSubmatch(line); len(match) == 3 {
			pName := match[1]
			pVal := cleanQuotes(match[2])
			profileRaw[pName] = pVal
			continue
		}

		// PROFILE_DESCR[...]
		if match := reDescrDecl.FindStringSubmatch(line); len(match) == 3 {
			pName := match[1]
			pVal := cleanQuotes(match[2])
			profileDescr[pName] = pVal
			continue
		}

		// Parse variable assignments (including multiple per line separated by ;)
		parseVarAssignments(line, vars)
	}

	modelPath := expandVars(vars["MODEL"], vars)
	if modelPath == "" && vars["MODEL_Q2"] != "" {
		modelPath = expandVars(vars["MODEL_Q2"], vars)
	}
	if modelPath == "" && vars["MODEL_9B_Q4"] != "" {
		modelPath = expandVars(vars["MODEL_9B_Q4"], vars)
	}
	if modelPath == "" {
		return nil, fmt.Errorf("no MODEL variable found in script")
	}

	mmprojPath := expandVars(vars["MMPROJ"], vars)
	if mmprojPath == "" && vars["MMPROJ_Q2"] != "" {
		mmprojPath = expandVars(vars["MMPROJ_Q2"], vars)
	}
	if mmprojPath == "" && vars["MMPROJ_9B"] != "" {
		mmprojPath = expandVars(vars["MMPROJ_9B"], vars)
	}

	mtpPath := expandVars(vars["MTP"], vars)

	llamaBin := expandVars(vars["LLAMA_BIN"], vars)
	if llamaBin == "" {
		homeDir, _ := os.UserHomeDir()
		vkBin := filepath.Join(homeDir, "llama.cpp", "build-vk", "bin", "llama-server")
		if _, err := os.Stat(vkBin); err == nil {
			llamaBin = vkBin
		} else if _, err := exec.LookPath("llama-server"); err != nil {
			llamaBin = vkBin
		} else {
			llamaBin = "llama-server"
		}
	}

	engineID := "llama-vk"
	if strings.Contains(llamaBin, "sycl") || strings.Contains(filename, "sycl") {
		engineID = "llama-sycl"
	}

	port := 8080
	if pStr, ok := vars["PORT"]; ok {
		if pVal, err := strconv.Atoi(pStr); err == nil && pVal > 0 {
			port = pVal
		}
	}

	modelName := modelID
	if len(commentHeader) > 0 {
		header := commentHeader[0]
		header = strings.TrimPrefix(header, "Запуск llama-server с ")
		header = strings.TrimPrefix(header, filename+" — ")
		if idx := strings.Index(header, " на "); idx != -1 {
			header = header[:idx]
		}
		if idx := strings.Index(header, " через "); idx != -1 {
			header = header[:idx]
		}
		modelName = strings.TrimSpace(header)
	}

	res := &ParsedModel{
		ModelID:        modelID,
		ModelName:      modelName,
		EngineBinary:   llamaBin,
		EngineID:       engineID,
		ModelPath:      modelPath,
		MMProjPath:     mmprojPath,
		MTPPath:        mtpPath,
		DefaultPort:    port,
		DefaultProfile: "default",
		Profiles:       make(map[string]ParsedProfile),
	}

	// Parse profiles
	for pName, raw := range profileRaw {
		useVision := false
		if strings.Contains(strings.ToLower(pName), "vision") ||
			strings.Contains(strings.ToLower(profileDescr[pName]), "vision") {
			useVision = true
		}

		prof := ParsedProfile{
			Name:        pName,
			Description: profileDescr[pName],
			CtxSize:     4096,
			Parallel:    1,
			KVType:      "q8_0",
			FlashAttn:   "auto",
			UseMTP:      true,
			UseVision:   useVision,
			EnableUI:    true,
			Tools:       "safe",
		}

		parts := strings.Fields(raw)
		if len(parts) > 1 && parts[0] == "_apply" {
			args := parts[1:]
			if len(args) >= 1 {
				if ctx, err := strconv.Atoi(args[0]); err == nil {
					prof.CtxSize = ctx
				}
			}
			for _, arg := range args {
				switch arg {
				case "q4_0", "q8_0", "f16":
					prof.KVType = arg
				case "on", "off", "auto":
					prof.FlashAttn = arg
				case "safe", "all":
					prof.Tools = arg
				}
			}
		}

		res.Profiles[pName] = prof
	}

	if len(res.Profiles) == 0 {
		res.Profiles["default"] = ParsedProfile{
			Name:        "default",
			Description: "Стандартный профиль",
			CtxSize:     4096,
			Parallel:    1,
			KVType:      "q8_0",
			FlashAttn:   "auto",
			UseMTP:      true,
			UseVision:   false,
			EnableUI:    true,
			Tools:       "safe",
		}
	}

	return res, nil
}

func ImportDirectory(ctx context.Context, database *db.DB, dirPath string) (*ImportResult, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read dir: %w", err)
	}

	res := &ImportResult{}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, "-start") {
			continue
		}

		res.TotalScripts++
		fullPath := filepath.Join(dirPath, name)
		contentBytes, err := os.ReadFile(fullPath)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: read error: %v", name, err))
			continue
		}

		parsed, err := ParseScriptContent(name, string(contentBytes))
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: parse error: %v", name, err))
			continue
		}

		// Ensure Engine
		engineName := "Llama.cpp Vulkan"
		if parsed.EngineID == "llama-sycl" {
			engineName = "Llama.cpp Intel SYCL"
		}
		_ = database.SaveEngine(ctx, db.Engine{
			ID:         parsed.EngineID,
			Name:       engineName,
			BinaryPath: parsed.EngineBinary,
		})

		// Save Model
		existingModel, _ := database.GetModel(ctx, parsed.ModelID)
		isFav := false
		if existingModel != nil {
			isFav = existingModel.IsFavorite
		}

		model := db.Model{
			ID:             parsed.ModelID,
			Name:           parsed.ModelName,
			EngineID:       parsed.EngineID,
			ModelPath:      parsed.ModelPath,
			MMProjPath:     parsed.MMProjPath,
			MTPPath:        parsed.MTPPath,
			DefaultPort:    parsed.DefaultPort,
			DefaultProfile: parsed.DefaultProfile,
			IsFavorite:     isFav,
		}
		if err := database.SaveModel(ctx, model); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: save model error: %v", name, err))
			continue
		}

		// Save Profiles
		for _, prof := range parsed.Profiles {
			p := db.Profile{
				ModelID:     parsed.ModelID,
				Name:        prof.Name,
				Description: prof.Description,
				CtxSize:     prof.CtxSize,
				Parallel:    prof.Parallel,
				KVType:      prof.KVType,
				FlashAttn:   prof.FlashAttn,
				UseMTP:      prof.UseMTP,
				UseVision:   prof.UseVision,
				EnableUI:    prof.EnableUI,
				Tools:       prof.Tools,
				ExtraArgs:   prof.ExtraArgs,
			}
			if err := database.SaveProfile(ctx, p); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: save profile '%s' error: %v", name, prof.Name, err))
			}
		}

		res.ImportedModels++
	}

	return res, nil
}
