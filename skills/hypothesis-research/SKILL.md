---
name: hypothesis-research
description: Evaluate a testable hypothesis with browser research, competing explanations, and a sourced verdict.
---

# Hypothesis research

1. Restate the claim in testable terms. Specify the population or setting, the outcome, and the comparison. State what observation would contradict the claim. Ask the user only when a missing detail would materially change the investigation.
2. Plan multi-step work with `todo_write`. Use `web_search` to discover evidence and `browse` to verify the actual source. Search separately for supporting evidence, contradictory evidence, and plausible alternative explanations. Use `research` for independent lines of inquiry when the tool is available. Give each subagent its own question and evidence criteria.
3. Prefer primary studies, datasets, and official documentation. Check what the evidence actually measured, the comparison it used, and limits on applying it to the hypothesis. Distinguish absence of evidence from evidence against the claim. Use browser interaction only when needed to read a source.
4. Give a verdict of supported, mixed, unsupported, or insufficient evidence. Explain the strongest supporting and opposing findings and what could change the verdict. Cite the URLs you actually verified. Update the plan when the research is complete.
