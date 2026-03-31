package main

import (
    "encoding/json"
    "fmt"
    "go/ast"
    "go/parser"
    "go/token"
    "os"
    "path/filepath"
    "sort"
    "strings"
)

type Finding struct {
    File string `json:"file"`
    Line int `json:"line"`
    Code string `json:"code"`
    Rule string `json:"rule"`
    Severity string `json:"severity"`
    Fix string `json:"fix"`
}

type funcCtx struct {
    lines []string
}

func main() {
    roots := []string{"cmd", "pkg", "tests"}
    findings := make([]Finding, 0)
    fset := token.NewFileSet()

    for _, root := range roots {
        _ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
            if err != nil {
                return err
            }
            if info.IsDir() {
                base := info.Name()
                if base == ".git" || base == ".agent" || base == "node_modules" || base == "dist" {
                    return filepath.SkipDir
                }
                return nil
            }
            if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".pb.go") {
                return nil
            }

            src, err := os.ReadFile(path)
            if err != nil {
                return err
            }
            file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
            if err != nil {
                return err
            }
            lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")

            ast.Inspect(file, func(n ast.Node) bool {
                switch fn := n.(type) {
                case *ast.FuncDecl:
                    findings = append(findings, inspectFunction(fset, path, lines, fn.Type, fn.Body, fn.Pos(), fn.End(), signatureLine(lines, fset.Position(fn.Pos()).Line), false)...)
                    return true
                case *ast.FuncLit:
                    findings = append(findings, inspectFunction(fset, path, lines, fn.Type, fn.Body, fn.Pos(), fn.End(), signatureLine(lines, fset.Position(fn.Pos()).Line), true)...)
                    return true
                default:
                    return true
                }
            })
            return nil
        })
    }

    sort.Slice(findings, func(i, j int) bool {
        if findings[i].File != findings[j].File {
            return findings[i].File < findings[j].File
        }
        if findings[i].Line != findings[j].Line {
            return findings[i].Line < findings[j].Line
        }
        return findings[i].Rule < findings[j].Rule
    })

    data, err := json.Marshal(findings)
    if err != nil {
        panic(err)
    }
    fmt.Println(string(data))
}

func inspectFunction(fset *token.FileSet, path string, lines []string, fnType *ast.FuncType, body *ast.BlockStmt, pos, end token.Pos, sig string, isLiteral bool) []Finding {
    if body == nil {
        return nil
    }
    findings := make([]Finding, 0, 2)
    startLine := fset.Position(pos).Line
    endLine := fset.Position(end).Line
    loc := endLine - startLine + 1
    if loc >= 50 {
        findings = append(findings, Finding{
            File: filepath.ToSlash(path),
            Line: startLine,
            Code: truncate(sig),
            Rule: "CQ-3: Functions MUST be under 50 LOC",
            Severity: "MEDIUM",
            Fix: fmt.Sprintf("Split this %d-line function into smaller helpers under 50 LOC", loc),
        })
    }

    line, snippet, ok := findDeepNesting(fset, lines, body.List, 0)
    if ok {
        findings = append(findings, Finding{
            File: filepath.ToSlash(path),
            Line: line,
            Code: truncate(snippet),
            Rule: "CQ-4: Nesting beyond 4 levels FORBIDDEN",
            Severity: "MEDIUM",
            Fix: "Flatten control flow with guard clauses or extracted helpers",
        })
    }
    _ = fnType
    _ = isLiteral
    return findings
}

func findDeepNesting(fset *token.FileSet, lines []string, stmts []ast.Stmt, depth int) (int, string, bool) {
    for _, stmt := range stmts {
        if line, snippet, ok := inspectStmt(fset, lines, stmt, depth); ok {
            return line, snippet, true
        }
    }
    return 0, "", false
}

func inspectStmt(fset *token.FileSet, lines []string, stmt ast.Stmt, depth int) (int, string, bool) {
    switch s := stmt.(type) {
    case *ast.BlockStmt:
        return findDeepNesting(fset, lines, s.List, depth)
    case *ast.IfStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        if line, snippet, ok := inspectStmt(fset, lines, s.Body, newDepth); ok {
            return line, snippet, true
        }
        if s.Else != nil {
            switch elseNode := s.Else.(type) {
            case *ast.IfStmt:
                if line, snippet, ok := inspectStmt(fset, lines, elseNode, depth); ok {
                    return line, snippet, true
                }
            default:
                if line, snippet, ok := inspectStmt(fset, lines, elseNode.(ast.Stmt), newDepth); ok {
                    return line, snippet, true
                }
            }
        }
    case *ast.ForStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        return inspectStmt(fset, lines, s.Body, newDepth)
    case *ast.RangeStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        return inspectStmt(fset, lines, s.Body, newDepth)
    case *ast.SwitchStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        for _, clause := range s.Body.List {
            cc := clause.(*ast.CaseClause)
            if line, snippet, ok := findDeepNesting(fset, lines, cc.Body, newDepth); ok {
                return line, snippet, true
            }
        }
    case *ast.TypeSwitchStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        for _, clause := range s.Body.List {
            cc := clause.(*ast.CaseClause)
            if line, snippet, ok := findDeepNesting(fset, lines, cc.Body, newDepth); ok {
                return line, snippet, true
            }
        }
    case *ast.SelectStmt:
        newDepth := depth + 1
        if newDepth > 4 {
            return posSnippet(fset, lines, s.Pos())
        }
        for _, clause := range s.Body.List {
            cc := clause.(*ast.CommClause)
            if line, snippet, ok := findDeepNesting(fset, lines, cc.Body, newDepth); ok {
                return line, snippet, true
            }
        }
    case *ast.LabeledStmt:
        return inspectStmt(fset, lines, s.Stmt, depth)
    }
    return 0, "", false
}

func posSnippet(fset *token.FileSet, lines []string, pos token.Pos) (int, string, bool) {
    line := fset.Position(pos).Line
    if line <= 0 || line > len(lines) {
        return line, "", true
    }
    return line, strings.TrimSpace(lines[line-1]), true
}

func signatureLine(lines []string, line int) string {
    if line <= 0 || line > len(lines) {
        return ""
    }
    return strings.TrimSpace(lines[line-1])
}

func truncate(s string) string {
    s = strings.TrimSpace(strings.ReplaceAll(s, "\t", " "))
    if len(s) <= 100 {
        return s
    }
    return s[:100]
}
