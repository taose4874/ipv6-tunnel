package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

type bigTheme struct{}

func (t bigTheme) Color(name fyne.ThemeColorName, variant fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameInputBackground:
		return color.White
	case theme.ColorNameForeground:
		return color.Black
	case theme.ColorNamePlaceHolder:
		return color.Gray{Y: 0x88}
	case theme.ColorNameDisabled:
		return color.Black
	case "log-success":
		return color.NRGBA{R: 0x4C, G: 0xAF, B: 0x50, A: 0xFF}
	case "log-warn":
		return color.NRGBA{R: 0xFF, G: 0x98, B: 0x00, A: 0xFF}
	case "log-error":
		return color.NRGBA{R: 0xF4, G: 0x43, B: 0x36, A: 0xFF}
	case "log-info":
		return color.NRGBA{R: 0xDD, G: 0xDD, B: 0xDD, A: 0xFF}
	default:
		return theme.DefaultTheme().Color(name, variant)
	}
}

func (t bigTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (t bigTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (t bigTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 16
	case theme.SizeNameHeadingText:
		return 20
	case theme.SizeNameSubHeadingText:
		return 18
	case theme.SizeNameCaptionText:
		return 14
	case theme.SizeNameInputBorder:
		return 2
	case theme.SizeNameScrollBar:
		return 14
	case theme.SizeNameScrollBarSmall:
		return 4
	case theme.SizeNamePadding:
		return 8
	case theme.SizeNameInnerPadding:
		return 6
	case theme.SizeNameSeparatorThickness:
		return 1
	default:
		return theme.DefaultTheme().Size(name)
	}
}