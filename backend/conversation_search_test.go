package main

import "testing"

func TestDetectNeedMoreSearchExtractsQuery(t *testing.T) {
	reply := "Before continuing I need more evidence.\n\n<<NEED_MORE_SEARCH: easy carrot recipe with full ingredients and step by step instructions>>\n"

	got := detectNeedMoreSearch(reply)
	want := "easy carrot recipe with full ingredients and step by step instructions"
	if got != want {
		t.Fatalf("detectNeedMoreSearch() = %q, want %q", got, want)
	}
}

func TestDetectNeedMoreSearchWithoutClosingTag(t *testing.T) {
	// Small models often omit the closing >>
	reply := "<<NEED_MORE_SEARCH: recette carottes économique pas carottes vichy simple pas cher"

	got := detectNeedMoreSearch(reply)
	want := "recette carottes économique pas carottes vichy simple pas cher"
	if got != want {
		t.Fatalf("detectNeedMoreSearch() = %q, want %q", got, want)
	}
}

func TestFollowUpSearchTriggerFrenchRecipeFallback(t *testing.T) {
	request := "parcours les sources, suis des liens s'il le faut ou refais une recherche pour me trouver une recette a base de carottes facile"
	reply := `Les sources actuelles que j'ai analysees ne fournissent que des titres et des listes de recettes, sans les etapes detaillees de preparation que vous recherchez.

Pour obtenir une recette complete, je vous recommande de cliquer directement sur l'un de ces liens qui contiennent les instructions completes.`

	got := followUpSearchTrigger(request, reply)
	if got != "fallback_llm_deflection" {
		t.Fatalf("followUpSearchTrigger() = %q, want %q", got, "fallback_llm_deflection")
	}
}

func TestFollowUpSearchTriggerDoesNotFireWithoutSearchIntent(t *testing.T) {
	request := "resume les sources actuelles sur les carottes"
	reply := "The current sources do not provide enough detail to answer beyond a high-level summary."

	got := followUpSearchTrigger(request, reply)
	if got != "" {
		t.Fatalf("followUpSearchTrigger() = %q, want empty trigger", got)
	}
}

func TestFollowUpSearchTriggerDeflectionAlwaysFires(t *testing.T) {
	// LLM deflects to external website — should trigger regardless of user's search intent.
	request := "donne-moi une autre recette de carottes"
	reply := `Le contexte actuel ne contient pas d'autre recette détaillée. La seule recette complète fournie est celle des Carottes Vichy (glacées).

Pour trouver une autre recette économique et simple, je vous suggère de consulter directement :

    Marmiton : Tapez "recette carottes économique" pour trouver des variantes.

Souhaitez-vous que je vous aide à reformuler une recherche spécifique pour ces sites ?`

	got := followUpSearchTrigger(request, reply)
	if got != "fallback_llm_deflection" {
		t.Fatalf("followUpSearchTrigger() = %q, want %q", got, "fallback_llm_deflection")
	}
}

func TestFollowUpSearchTriggerImplicitSearchIntent(t *testing.T) {
	// User asks for "another recipe" (implicit search need) + LLM says context insufficient
	request := "donne-moi une autre recette de carottes"
	reply := "Le contexte actuel ne contient pas d'autre recette. La seule recette complète est celle des Carottes Vichy."

	got := followUpSearchTrigger(request, reply)
	if got != "fallback_insufficient_context" {
		t.Fatalf("followUpSearchTrigger() = %q, want %q", got, "fallback_insufficient_context")
	}
}