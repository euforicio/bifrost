import { describe, expect, test } from "vitest";
import {
	analyzerConfigSchema,
	countCanonicalSemanticPhrases,
	DEFAULT_FORM_VALUES,
	shouldSeedLLMPrompt,
	toAnalyzerPayload,
	toFormValues,
} from "./formSchema";

describe("fallback prompt initialization", () => {
	test("initializes an untouched empty prompt", () => {
		expect(shouldSeedLLMPrompt(true, "default", "", false)).toBe(true);
	});
	test("does not refill an intentionally cleared prompt, including a late default response", () => {
		expect(shouldSeedLLMPrompt(true, "default", "", true)).toBe(false);
	});
	test("does not replace custom text or initialize a disabled fallback", () => {
		expect(shouldSeedLLMPrompt(true, "default", "custom", false)).toBe(false);
		expect(shouldSeedLLMPrompt(false, "default", "", false)).toBe(false);
		expect(shouldSeedLLMPrompt(true, "", "", false)).toBe(false);
	});
});

function phraseList(prefix: string, count: number): string[] {
	return Array.from({ length: count }, (_, index) => `${prefix}-${index}`);
}

function formValues(simpleCount: number, semantic: boolean) {
	return {
		...DEFAULT_FORM_VALUES,
		keywords: {
			simple_keywords: phraseList("simple", simpleCount),
			medium_keywords: ["medium"],
			complex_keywords: ["complex"],
		},
		semantic: semantic
			? { ...DEFAULT_FORM_VALUES.semantic, provider: "openai", embedding_model: "text-embedding-3-small" }
			: { ...DEFAULT_FORM_VALUES.semantic },
	};
}

describe("semantic complexity phrase limit", () => {
	test("accepts exactly 750 canonical phrases", () => {
		expect(analyzerConfigSchema.safeParse(formValues(748, true)).success).toBe(true);
	});

	test("rejects 751 canonical phrases with tier counts", () => {
		const result = analyzerConfigSchema.safeParse(formValues(749, true));
		expect(result.success).toBe(false);
		if (result.success) return;
		expect(result.error.issues.some((issue) => issue.message.includes("751 phrases (Simple=749, Medium=1, Complex=1)"))).toBe(true);
	});

	test("does not count blanks and same-tier duplicates twice", () => {
		const values = formValues(748, true);
		values.keywords.simple_keywords.push(" SIMPLE-0 ", "");
		expect(countCanonicalSemanticPhrases(values.keywords).total).toBe(750);
		expect(analyzerConfigSchema.safeParse(values).success).toBe(true);
	});

	test("does not cap a form that will omit the semantic block", () => {
		expect(analyzerConfigSchema.safeParse(formValues(750, false)).success).toBe(true);
	});
});

describe("Jev configuration compatibility", () => {
	test("leaves an unconfigured Jev classifier empty", () => {
		const values = formValues(1, false);
		expect(analyzerConfigSchema.safeParse(values).success).toBe(true);
		expect(toAnalyzerPayload(values).jev).toBeUndefined();
	});
	test("preserves a versioned Jev model and session routing", () => {
		const values = formValues(1, false);
		values.jev = { ...values.jev, provider: "typesafe", model: "jev-1.13.0" };
		values.session = { enabled: true };
		expect(analyzerConfigSchema.safeParse(values).success).toBe(true);
		const restored = toFormValues(toAnalyzerPayload(values));
		expect(restored.jev).toEqual(values.jev);
		expect(restored.session).toEqual(values.session);
	});
});