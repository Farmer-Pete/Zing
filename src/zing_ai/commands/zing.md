
<objective>
Thin alias that delegates to `/zing:new`. This skill simply forwards to `zing:new`, which handles file selection and kicks off the rest of the pipeline via direct chaining.
</objective>

<process>

<step name="delegate">
Check if `$ARGUMENTS` was provided:

- If `$ARGUMENTS` is provided: invoke `Skill(skill: 'zing:new', args: '$ARGUMENTS')`
- If no `$ARGUMENTS`: invoke `Skill(skill: 'zing:new')` with no args (zing:new already handles file selection)

Before chaining to the next skill, print an excited sentence containing "Zing!" with a lightning bolt-related emoji (e.g. ⚡). Vary the sentence each time — don't repeat the same one.
</step>

</process>
