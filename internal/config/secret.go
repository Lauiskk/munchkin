package config

// Secret é um valor sensível que se recusa a ser impresso.
//
// Existe porque a forma mais comum de vazar credencial não é ataque: é um
// `log.Info("config", "cfg", cfg)` durante uma depuração, ou um `%+v` num erro
// que sobe até o log estruturado. Um tipo que redige a si mesmo torna esse
// vazamento impossível por construção, em vez de depender de ninguém escrever
// a linha errada.
//
// Para usar o valor de fato, é preciso chamar Reveal() — o que é explícito no
// código e fácil de auditar por grep.
type Secret string

// String satisfaz fmt.Stringer e é o que %v e %s imprimem.
func (s Secret) String() string { return secretPlaceholder }

// GoString satisfaz fmt.GoStringer e é o que %#v imprime.
func (s Secret) GoString() string { return secretPlaceholder }

// MarshalJSON impede que o valor apareça em qualquer serialização JSON.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + secretPlaceholder + `"`), nil
}

// MarshalText cobre os codificadores que preferem texto a JSON.
func (s Secret) MarshalText() ([]byte, error) { return []byte(secretPlaceholder), nil }

// Reveal devolve o valor real. É o único caminho, e é deliberadamente verboso.
func (s Secret) Reveal() string { return string(s) }

// IsEmpty informa se o segredo não foi definido.
func (s Secret) IsEmpty() bool { return s == "" }

const secretPlaceholder = "[redigido]"
