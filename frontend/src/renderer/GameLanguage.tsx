import { gameLanguageOverrides } from '../shared/game-language';

function label(locale: string): string {
  try { return `${new Intl.DisplayNames(['en'], { type: 'language' }).of(locale)} (${locale})`; }
  catch { return locale; }
}

export function GameLanguage({ value, locales, onChange }: { value: string; locales: string[]; onChange: (locale: string) => void }) {
  const known = new Set(locales.map(locale => locale.toLowerCase()));
  const overrides = gameLanguageOverrides.filter(locale => !known.has(locale.toLowerCase()));
  const custom = Boolean(value) && !known.has(value.toLowerCase()) && !overrides.some(locale => locale.toLowerCase() === value.toLowerCase());
  return <>
    <label>Game language<select value={value} onChange={event => onChange(event.target.value)}>
      <option value="">System default</option>
      {locales.length ? <optgroup label="Package-listed languages">{locales.map(locale => <option key={locale} value={locale}>{label(locale)}</option>)}</optgroup> : null}
      <optgroup label="Language overrides (support varies)">{overrides.map(locale => <option key={locale} value={locale}>{label(locale)}</option>)}</optgroup>
      {custom ? <option value={value}>{label(value)}</option> : null}
    </select></label>
    <small>Package languages may be incomplete. Overrides request a language but do not install translations or voice packs. The game must support it.</small>
  </>;
}
