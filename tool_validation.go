package main

import (
	"errors"
	"io"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

type argumentValidator interface {
	Validate(interface{}) error
}

func compileToolSchema(tool chatTool) (argumentValidator, error) {
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.AssertFormat = true
	compiler.LoadURL = func(string) (io.ReadCloser, error) {
		return nil, errors.New("external schema resources are not permitted")
	}
	const resource = "https://cursor-tool.invalid/schema.json"
	schema := tool.SchemaText
	if schema == "" {
		schema = string(encodeJSON(tool.Schema))
	}
	if len(schema) > 1<<20 {
		return nil, failure(400, "Tool schema exceeds the 1 MiB limit")
	}
	if err := compiler.AddResource(resource, strings.NewReader(schema)); err != nil {
		return nil, failure(400, "Invalid strict JSON Schema for tool %q", tool.Name)
	}
	compiled, err := compiler.Compile(resource)
	if err != nil {
		// Compiler errors can contain enum/const values or external URLs. Do not
		// echo caller schema contents in logs or API errors.
		return nil, failure(400, "Invalid strict JSON Schema or external reference for tool %q", tool.Name)
	}
	return compiled, nil
}

func validateToolArguments(tool chatTool, args map[string]any) error {
	if !tool.Strict {
		return nil
	}
	validator := tool.validator
	if validator == nil {
		var err error
		validator, err = compileToolSchema(tool)
		if err != nil {
			return err
		}
	}
	if err := validator.Validate(args); err != nil {
		return failure(502, "Cursor returned arguments that violate the strict schema for tool %q", tool.Name)
	}
	return nil
}

// Called before exposing any tool in a completed batch. These checks validate
// model output; they neither execute a tool nor repair its name or arguments.
func validateToolOutputBatch(choice any, parallel *bool, tools []chatTool, calls []map[string]any) error {
	forced := str(object(object(choice)["function"])["name"])
	if (choice == "required" || forced != "") && len(calls) == 0 {
		return failure(502, "Cursor completed without the required function tool call")
	}
	if forced != "" && len(calls) != 1 {
		return failure(502, "Cursor returned more than one call for the forced tool_choice")
	}
	if choice == "none" && len(calls) > 0 {
		return failure(502, "Cursor returned a tool call when tool_choice is none")
	}
	if parallel != nil && !*parallel && len(calls) > 1 {
		return failure(502, "Cursor returned multiple tool calls with parallel_tool_calls=false")
	}
	for _, call := range calls {
		function := object(call["function"])
		name := str(function["name"])
		if forced != "" && name != forced {
			return failure(502, "Cursor returned a different function than the forced tool_choice")
		}
		var tool *chatTool
		for i := range tools {
			if tools[i].Name == name {
				tool = &tools[i]
				break
			}
		}
		if tool == nil {
			return failure(502, "Cursor returned an undeclared function tool")
		}
		args, err := nativeArgs(function["arguments"])
		if err != nil {
			return failure(502, "Cursor returned invalid function arguments")
		}
		if err := validateToolArguments(*tool, args); err != nil {
			return err
		}
	}
	return nil
}
